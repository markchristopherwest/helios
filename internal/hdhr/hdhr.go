// Package hdhr talks to SiliconDust HDHomeRun tuners two ways:
//
//  1. UDP broadcast discovery on :65001 using the libhdhomerun wire format
//     (type 0x0002 request / 0x0003 reply, TLV payload, trailing CRC32-LE).
//  2. The HTTP JSON API every modern firmware exposes: /discover.json and
//     /lineup.json. Lineup entries carry ready-to-GET MPEG-TS stream URLs
//     (http://<tuner>:5004/auto/v<chan>); the tuner does its own tuner
//     allocation, so recording is just an HTTP GET copied to disk.
package hdhr

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/markchristopherwest/helios/internal/store"
)

const (
	typeDiscoverReq = 0x0002
	typeDiscoverRpy = 0x0003
	tagDeviceType   = 0x01
	tagDeviceID     = 0x02
	tagBaseURL      = 0x2A
	wildcard        = 0xFFFFFFFF
)

type Device struct {
	DeviceID   string `json:"DeviceID"`
	LocalIP    string `json:"LocalIP"`
	BaseURL    string `json:"BaseURL"`
	LineupURL  string `json:"LineupURL"`
	TunerCount int    `json:"TunerCount"`
	ModelNum   string `json:"ModelNumber"`
	Firmware   string `json:"FirmwareVersion"`
}

// discoverPacket builds a wildcard discover request per hdhomerun_pkt.h.
func discoverPacket() []byte {
	payload := []byte{
		tagDeviceType, 4, 0, 0, 0, 0,
		tagDeviceID, 4, 0, 0, 0, 0,
	}
	binary.BigEndian.PutUint32(payload[2:6], wildcard)
	binary.BigEndian.PutUint32(payload[8:12], wildcard)
	pkt := make([]byte, 4+len(payload)+4)
	binary.BigEndian.PutUint16(pkt[0:2], typeDiscoverReq)
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(payload)))
	copy(pkt[4:], payload)
	crc := crc32.ChecksumIEEE(pkt[:4+len(payload)])
	binary.LittleEndian.PutUint32(pkt[4+len(payload):], crc)
	return pkt
}

func parseReply(b []byte) (id, baseURL string, ok bool) {
	if len(b) < 8 || binary.BigEndian.Uint16(b[0:2]) != typeDiscoverRpy {
		return
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if len(b) < 4+n+4 {
		return
	}
	want := binary.LittleEndian.Uint32(b[4+n:])
	if crc32.ChecksumIEEE(b[:4+n]) != want {
		return
	}
	p := b[4 : 4+n]
	for len(p) >= 2 {
		tag, l := p[0], int(p[1])
		p = p[2:]
		if len(p) < l {
			return
		}
		switch tag {
		case tagDeviceID:
			if l == 4 {
				id = fmt.Sprintf("%08X", binary.BigEndian.Uint32(p[:4]))
			}
		case tagBaseURL:
			baseURL = string(p[:l])
		}
		p = p[l:]
	}
	return id, baseURL, baseURL != ""
}

// DiscoverUDP broadcasts and collects replies for ~2s.
func DiscoverUDP(ctx context.Context) ([]Device, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: 65001}
	if _, err := conn.WriteToUDP(discoverPacket(), dst); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	seen := map[string]bool{}
	var devs []Device
	buf := make([]byte, 2048)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // timeout ends collection
		}
		if _, base, ok := parseReply(buf[:n]); ok && !seen[base] {
			seen[base] = true
			if d, err := fetchDiscover(ctx, base); err == nil {
				d.LocalIP = addr.IP.String()
				devs = append(devs, d)
			}
		}
	}
	return devs, nil
}

func fetchDiscover(ctx context.Context, baseURL string) (Device, error) {
	var d Device
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(baseURL, "/")+"/discover.json", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return d, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return d, fmt.Errorf("discover.json: %s", res.Status)
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&d)
	if d.BaseURL == "" {
		d.BaseURL = baseURL
	}
	return d, err
}

type lineupEntry struct {
	GuideNumber string `json:"GuideNumber"`
	GuideName   string `json:"GuideName"`
	URL         string `json:"URL"`
	HD          int    `json:"HD"`
}

// Client caches the resolved device + lineup and refreshes on demand.
type Client struct {
	DB *store.DB

	mu       sync.RWMutex
	device   *Device
	channels []store.Channel
}

// Refresh resolves the tuner (Settings.HDHRIP wins, else UDP discovery) and
// pulls its channel lineup.
func (c *Client) Refresh(ctx context.Context) error {
	set := c.DB.Settings()
	var dev Device
	var err error
	if ip := strings.TrimSpace(set.HDHRIP); ip != "" {
		dev, err = fetchDiscover(ctx, "http://"+ip)
		if err != nil {
			return fmt.Errorf("hdhr at %s: %w", ip, err)
		}
	} else {
		devs, derr := DiscoverUDP(ctx)
		if derr != nil {
			return derr
		}
		if len(devs) == 0 {
			return fmt.Errorf("no HDHomeRun found via broadcast; set hdhrIp in settings")
		}
		dev = devs[0]
	}
	lineupURL := dev.LineupURL
	if lineupURL == "" {
		lineupURL = strings.TrimRight(dev.BaseURL, "/") + "/lineup.json"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", lineupURL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var entries []lineupEntry
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&entries); err != nil {
		return fmt.Errorf("lineup.json: %w", err)
	}
	chans := make([]store.Channel, 0, len(entries))
	for _, e := range entries {
		chans = append(chans, store.Channel{
			GuideNumber: e.GuideNumber, GuideName: e.GuideName,
			URL: e.URL, HD: e.HD == 1,
		})
	}
	c.mu.Lock()
	c.device, c.channels = &dev, chans
	c.mu.Unlock()
	return nil
}

func (c *Client) Device() *Device {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.device
}

func (c *Client) Channels() []store.Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]store.Channel, len(c.channels))
	copy(out, c.channels)
	return out
}

// StreamURL returns the MPEG-TS URL for a GuideNumber ("" if unknown).
func (c *Client) StreamURL(guideNumber string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ch := range c.channels {
		if ch.GuideNumber == guideNumber {
			return ch.URL
		}
	}
	if c.device != nil {
		// fall back to the auto endpoint; the tuner picks a free tuner itself
		return strings.TrimRight(c.device.BaseURL, "/") + "/auto/v" + guideNumber
	}
	return ""
}
