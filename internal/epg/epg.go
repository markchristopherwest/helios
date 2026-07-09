// Package epg loads an XMLTV guide (URL or local file — pair with zap2xml,
// Schedules Direct via tv_grab_zz_sdjson, or your grabber of choice) and maps
// its channels onto the HDHomeRun lineup by guide number or name so airings
// carry the tuner's GuideNumber as ChannelID.
package epg

import (
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/markchristopherwest/helios/internal/store"
)

type Airing struct {
	ChannelID   string    `json:"channelId"` // HDHR GuideNumber
	Title       string    `json:"title"`
	Subtitle    string    `json:"subtitle,omitempty"`
	Description string    `json:"description,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
}

type xmltv struct {
	Channels []struct {
		ID    string   `xml:"id,attr"`
		Names []string `xml:"display-name"`
	} `xml:"channel"`
	Programmes []struct {
		Channel  string `xml:"channel,attr"`
		Start    string `xml:"start,attr"`
		Stop     string `xml:"stop,attr"`
		Title    string `xml:"title"`
		SubTitle string `xml:"sub-title"`
		Desc     string `xml:"desc"`
	} `xml:"programme"`
}

type Guide struct {
	DB *store.DB

	mu        sync.RWMutex
	airings   []Airing
	byChannel map[string][]Airing
	updated   time.Time
}

// Refresh downloads/reads XMLTV and rebuilds the in-memory guide.
// channels is the current HDHR lineup used for channel matching.
func (g *Guide) Refresh(ctx context.Context, channels []store.Channel) error {
	src := strings.TrimSpace(g.DB.Settings().XMLTVURL)
	if src == "" {
		return nil // guide is optional: manual + time-based recording still works
	}
	rc, err := open(ctx, src)
	if err != nil {
		return err
	}
	defer rc.Close()

	var doc xmltv
	if err := xml.NewDecoder(rc).Decode(&doc); err != nil {
		return fmt.Errorf("xmltv parse: %w", err)
	}

	// xmltv channel id -> HDHR GuideNumber. Match display-names against the
	// lineup's number ("7.1") and name ("KABCDT"), case-insensitively.
	byNum := map[string]string{}
	byName := map[string]string{}
	for _, c := range channels {
		byNum[c.GuideNumber] = c.GuideNumber
		byName[strings.ToLower(c.GuideName)] = c.GuideNumber
	}
	chmap := map[string]string{}
	for _, c := range doc.Channels {
		for _, n := range c.Names {
			key := strings.ToLower(strings.TrimSpace(n))
			if gn, ok := byNum[strings.TrimSpace(n)]; ok {
				chmap[c.ID] = gn
				break
			}
			if gn, ok := byName[key]; ok {
				chmap[c.ID] = gn
				break
			}
		}
	}

	var airings []Airing
	for _, p := range doc.Programmes {
		gn, ok := chmap[p.Channel]
		if !ok {
			continue
		}
		start, err1 := parseXMLTVTime(p.Start)
		end, err2 := parseXMLTVTime(p.Stop)
		if err1 != nil || err2 != nil || !end.After(start) {
			continue
		}
		airings = append(airings, Airing{
			ChannelID: gn, Title: strings.TrimSpace(p.Title),
			Subtitle: strings.TrimSpace(p.SubTitle), Description: strings.TrimSpace(p.Desc),
			Start: start, End: end,
		})
	}
	sort.Slice(airings, func(i, j int) bool { return airings[i].Start.Before(airings[j].Start) })
	byCh := map[string][]Airing{}
	for _, a := range airings {
		byCh[a.ChannelID] = append(byCh[a.ChannelID], a)
	}

	g.mu.Lock()
	g.airings, g.byChannel, g.updated = airings, byCh, time.Now()
	g.mu.Unlock()
	return nil
}

func open(ctx context.Context, src string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		req, _ := http.NewRequestWithContext(ctx, "GET", src, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return nil, fmt.Errorf("xmltv fetch: %s", res.Status)
		}
		rc = res.Body
	} else {
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		rc = f
	}
	if strings.HasSuffix(src, ".gz") {
		gz, err := gzip.NewReader(rc)
		if err != nil {
			rc.Close()
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{gz, rc}, nil
	}
	return rc, nil
}

func parseXMLTVTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"20060102150405 -0700", "20060102150405"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad xmltv time %q", s)
}

// Window returns airings overlapping [from, to] per channel.
func (g *Guide) Window(from, to time.Time) map[string][]Airing {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := map[string][]Airing{}
	for ch, list := range g.byChannel {
		for _, a := range list {
			if a.End.After(from) && a.Start.Before(to) {
				out[ch] = append(out[ch], a)
			}
		}
	}
	return out
}

// Match returns future airings whose title equals-fold the rule title,
// optionally restricted to one channel.
func (g *Guide) Match(title, channelID string) []Airing {
	g.mu.RLock()
	defer g.mu.RUnlock()
	now := time.Now()
	var out []Airing
	for _, a := range g.airings {
		if a.End.Before(now) {
			continue
		}
		if channelID != "" && a.ChannelID != channelID {
			continue
		}
		if strings.EqualFold(a.Title, title) {
			out = append(out, a)
		}
	}
	return out
}

func (g *Guide) Updated() time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.updated
}
