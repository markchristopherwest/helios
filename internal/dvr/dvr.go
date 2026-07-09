// Package dvr is the recording engine. A 15s tick drives four jobs:
//
//   - match series rules against the guide and materialize scheduled recordings
//   - start captures that are due (airing start minus pre-pad)
//   - prune per-rule beyond Keep, and reap fully-watched recordings if enabled
//
// Capture is deliberately dumb and robust: HTTP GET the tuner's MPEG-TS and
// io.Copy to disk until end+post-pad. The HDHomeRun allocates its own tuners
// behind /auto/v<chan>; if all tuners are busy it answers 503 and we mark the
// recording failed. Post-capture: remux .ts -> .mkv (stream copy), then run
// the commercial pipeline per Settings.CommercialMode.
package dvr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/markchristopherwest/helios/internal/commercials"
	"github.com/markchristopherwest/helios/internal/epg"
	"github.com/markchristopherwest/helios/internal/hdhr"
	"github.com/markchristopherwest/helios/internal/media"
	"github.com/markchristopherwest/helios/internal/store"
)

type Engine struct {
	DB    *store.DB
	Guide *epg.Guide
	HDHR  *hdhr.Client

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func New(db *store.DB, g *epg.Guide, h *hdhr.Client) *Engine {
	return &Engine{DB: db, Guide: g, HDHR: h, active: map[string]context.CancelFunc{}}
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	e.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.tick(ctx)
		}
	}
}

func (e *Engine) tick(ctx context.Context) {
	e.matchRules()
	e.startDue(ctx)
	e.prune()
}

// matchRules creates scheduled recordings for future guide airings that match
// a rule. Dedupe key: rule + airing start + channel.
func (e *Engine) matchRules() {
	var rules []store.Rule
	existing := map[string]bool{}
	e.DB.Read(func(d *store.Data) {
		for _, r := range d.Rules {
			rules = append(rules, *r)
		}
		for _, rec := range d.Recordings {
			existing[rec.RuleID+"|"+rec.ChannelID+"|"+rec.Start.UTC().Format(time.RFC3339)] = true
		}
	})
	for _, rule := range rules {
		for _, a := range e.Guide.Match(rule.Title, rule.ChannelID) {
			key := rule.ID + "|" + a.ChannelID + "|" + a.Start.UTC().Format(time.RFC3339)
			if existing[key] {
				continue
			}
			existing[key] = true
			rec := &store.Recording{
				ID: newID(), Title: a.Title, Subtitle: a.Subtitle,
				Description: a.Description, ChannelID: a.ChannelID,
				ChannelName: e.channelName(a.ChannelID),
				Start:       a.Start, End: a.End,
				Status: "scheduled", RuleID: rule.ID, Added: time.Now(),
			}
			e.DB.Write(func(d *store.Data) { d.Recordings[rec.ID] = rec })
			log.Printf("dvr: scheduled %q %s", rec.Title, rec.Start.Format(time.Stamp))
		}
	}
}

func (e *Engine) channelName(guideNumber string) string {
	for _, c := range e.HDHR.Channels() {
		if c.GuideNumber == guideNumber {
			return c.GuideName
		}
	}
	return ""
}

func (e *Engine) startDue(ctx context.Context) {
	set := e.DB.Settings()
	pre := time.Duration(set.PrePadMin) * time.Minute
	post := time.Duration(set.PostPadMin) * time.Minute
	now := time.Now()

	var due []store.Recording
	e.DB.Read(func(d *store.Data) {
		for _, r := range d.Recordings {
			if r.Status == "scheduled" && !now.Before(r.Start.Add(-pre)) && now.Before(r.End.Add(post)) {
				due = append(due, *r)
			}
			if r.Status == "scheduled" && !now.Before(r.End.Add(post)) {
				r.Status, r.Error = "failed", "missed airing (server offline?)"
			}
		}
	})
	for _, rec := range due {
		rec := rec
		e.mu.Lock()
		if _, running := e.active[rec.ID]; running {
			e.mu.Unlock()
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		e.active[rec.ID] = cancel
		e.mu.Unlock()
		go func() {
			defer func() {
				e.mu.Lock()
				delete(e.active, rec.ID)
				e.mu.Unlock()
			}()
			e.record(rctx, rec.ID, post)
		}()
	}
}

// Cancel stops an in-flight capture or unschedules a pending one.
func (e *Engine) Cancel(id string) {
	e.mu.Lock()
	if c, ok := e.active[id]; ok {
		c()
	}
	e.mu.Unlock()
	e.DB.Write(func(d *store.Data) {
		if r, ok := d.Recordings[id]; ok && r.Status == "scheduled" {
			r.Status = "canceled"
		}
	})
}

var unsafeFS = regexp.MustCompile(`[^\w .()-]+`)

func safeName(s string) string {
	s = unsafeFS.ReplaceAllString(s, "_")
	return strings.TrimSpace(s)
}

func (e *Engine) record(ctx context.Context, id string, post time.Duration) {
	var rec store.Recording
	e.DB.Read(func(d *store.Data) {
		if r, ok := d.Recordings[id]; ok {
			rec = *r
		}
	})
	if rec.ID == "" {
		return
	}
	set := e.DB.Settings()
	url := e.HDHR.StreamURL(rec.ChannelID)
	if url == "" {
		e.fail(id, "no tuner/channel URL — is the HDHomeRun reachable?")
		return
	}

	dir := filepath.Join(set.RecordingsDir, safeName(rec.Title))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.fail(id, err.Error())
		return
	}
	base := fmt.Sprintf("%s - %s", safeName(rec.Title), rec.Start.Format("2006-01-02 15-04"))
	if rec.Subtitle != "" {
		base += " - " + safeName(rec.Subtitle)
	}
	tsPath := filepath.Join(dir, base+".ts")

	e.DB.Write(func(d *store.Data) {
		if r, ok := d.Recordings[id]; ok {
			r.Status, r.Path = "recording", tsPath
		}
	})
	log.Printf("dvr: recording %q -> %s", rec.Title, tsPath)

	stopAt := rec.End.Add(post)
	cctx, cancel := context.WithDeadline(ctx, stopAt)
	defer cancel()

	err := capture(cctx, url, tsPath)
	// hitting the deadline is the normal, successful end of a capture
	if err != nil && cctx.Err() == nil && ctx.Err() == nil {
		e.fail(id, err.Error())
		return
	}
	if ctx.Err() != nil && time.Now().Before(rec.End) {
		e.fail(id, "capture canceled")
		return
	}

	final := e.postProcess(context.Background(), id, tsPath, set)
	log.Printf("dvr: finished %q -> %s", rec.Title, final)
}

func capture(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("tuner: %s (all tuners busy?)", res.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, res.Body)
	return err
}

// postProcess: remux to mkv, probe, then commercial-scan per settings.
func (e *Engine) postProcess(ctx context.Context, id, tsPath string, set store.Settings) string {
	final := tsPath
	mkv := strings.TrimSuffix(tsPath, ".ts") + ".mkv"
	remux := exec.CommandContext(ctx, set.FFmpegPath, "-hide_banner", "-loglevel", "error",
		"-i", tsPath, "-map", "0:v:0", "-map", "0:a?", "-c", "copy", "-y", mkv)
	if err := remux.Run(); err == nil {
		os.Remove(tsPath)
		final = mkv
	} else {
		log.Printf("dvr: remux failed, keeping .ts: %v", err)
	}

	info, _ := media.Probe(ctx, set.FFprobePath, final)
	e.DB.Write(func(d *store.Data) {
		if r, ok := d.Recordings[id]; ok {
			r.Status, r.Path, r.Info = "done", final, info
			if set.CommercialMode != "off" {
				r.BreaksState = "pending"
			}
		}
	})

	if set.CommercialMode == "off" {
		return final
	}
	breaks, err := commercials.Detect(ctx, set, final)
	if err != nil {
		log.Printf("dvr: commercial detect: %v", err)
		e.DB.Write(func(d *store.Data) {
			if r, ok := d.Recordings[id]; ok {
				r.BreaksState = "failed"
			}
		})
		return final
	}
	log.Printf("dvr: %d ad break(s) in %s", len(breaks), filepath.Base(final))

	if set.CommercialMode == "delete" && len(breaks) > 0 {
		newPath, kept, err := commercials.Cut(ctx, set, final, info.DurationSec, breaks)
		if err != nil {
			log.Printf("dvr: cut failed, falling back to skip markers: %v", err)
		} else {
			ninfo, _ := media.Probe(ctx, set.FFprobePath, newPath)
			if ninfo.DurationSec == 0 {
				ninfo.DurationSec = kept
			}
			e.DB.Write(func(d *store.Data) {
				if r, ok := d.Recordings[id]; ok {
					r.Path, r.Info, r.Breaks, r.BreaksState = newPath, ninfo, nil, "cut"
				}
			})
			return newPath
		}
	}
	e.DB.Write(func(d *store.Data) {
		if r, ok := d.Recordings[id]; ok {
			r.Breaks, r.BreaksState = breaks, "ready"
		}
	})
	return final
}

func (e *Engine) fail(id, msg string) {
	log.Printf("dvr: %s failed: %s", id, msg)
	e.DB.Write(func(d *store.Data) {
		if r, ok := d.Recordings[id]; ok {
			r.Status, r.Error = "failed", msg
		}
	})
}

// RescanCommercials re-runs detection for an existing recording (API hook).
func (e *Engine) RescanCommercials(ctx context.Context, id string, andCut bool) error {
	var rec store.Recording
	e.DB.Read(func(d *store.Data) {
		if r, ok := d.Recordings[id]; ok {
			rec = *r
		}
	})
	if rec.Path == "" || rec.Status != "done" {
		return fmt.Errorf("recording not ready")
	}
	set := e.DB.Settings()
	breaks, err := commercials.Detect(ctx, set, rec.Path)
	if err != nil {
		return err
	}
	if andCut && len(breaks) > 0 {
		newPath, kept, err := commercials.Cut(ctx, set, rec.Path, rec.Info.DurationSec, breaks)
		if err != nil {
			return err
		}
		info, _ := media.Probe(ctx, set.FFprobePath, newPath)
		if info.DurationSec == 0 {
			info.DurationSec = kept
		}
		e.DB.Write(func(d *store.Data) {
			if r, ok := d.Recordings[id]; ok {
				r.Path, r.Info, r.Breaks, r.BreaksState = newPath, info, nil, "cut"
			}
		})
		return nil
	}
	e.DB.Write(func(d *store.Data) {
		if r, ok := d.Recordings[id]; ok {
			r.Breaks, r.BreaksState = breaks, "ready"
		}
	})
	return nil
}

// prune enforces Rule.Keep (oldest first) and reaps watched recordings when
// AutoDeleteWatched is on. Files are deleted from disk, not just the index.
func (e *Engine) prune() {
	set := e.DB.Settings()
	type victim struct{ id, path string }
	var victims []victim
	e.DB.Read(func(d *store.Data) {
		byRule := map[string][]*store.Recording{}
		for _, r := range d.Recordings {
			if r.Status == "done" && r.RuleID != "" {
				byRule[r.RuleID] = append(byRule[r.RuleID], r)
			}
			if set.AutoDeleteWatched && r.Status == "done" {
				if w, ok := d.Watch[r.ID]; ok && w.Done {
					victims = append(victims, victim{r.ID, r.Path})
				}
			}
		}
		for ruleID, recs := range byRule {
			rule, ok := d.Rules[ruleID]
			if !ok || rule.Keep <= 0 || len(recs) <= rule.Keep {
				continue
			}
			sort.Slice(recs, func(i, j int) bool { return recs[i].Start.Before(recs[j].Start) })
			for _, r := range recs[:len(recs)-rule.Keep] {
				victims = append(victims, victim{r.ID, r.Path})
			}
		}
	})
	if len(victims) == 0 {
		return
	}
	seen := map[string]bool{}
	e.DB.Write(func(d *store.Data) {
		for _, v := range victims {
			if seen[v.id] {
				continue
			}
			seen[v.id] = true
			delete(d.Recordings, v.id)
			delete(d.Watch, v.id)
		}
	})
	for _, v := range victims {
		if v.path != "" {
			if err := os.Remove(v.path); err != nil && !os.IsNotExist(err) {
				log.Printf("dvr: prune %s: %v", v.path, err)
			}
		}
	}
}
