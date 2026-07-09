// Package store is the single source of truth: all persistent state lives in
// one JSON document guarded by an RWMutex, flushed atomically (tmp+rename)
// with debounce. Deliberately no SQL: helios has zero external Go deps, and a
// homelab library (thousands of items, not millions) fits comfortably in
// memory. Every other package imports store; store imports only stdlib.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ---- domain types ----------------------------------------------------------

type MediaInfo struct {
	DurationSec float64 `json:"durationSec"`
	Container   string  `json:"container"`
	VCodec      string  `json:"vcodec"`
	ACodec      string  `json:"acodec"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	SizeBytes   int64   `json:"sizeBytes"`
}

type Movie struct {
	ID    string    `json:"id"`
	Path  string    `json:"path"`
	Title string    `json:"title"`
	Year  int       `json:"year,omitempty"`
	Added time.Time `json:"added"`
	Info  MediaInfo `json:"info"`
}

type Episode struct {
	ID      string    `json:"id"`
	Path    string    `json:"path"`
	Show    string    `json:"show"`
	Season  int       `json:"season"`
	Episode int       `json:"episode"`
	Title   string    `json:"title,omitempty"`
	Added   time.Time `json:"added"`
	Info    MediaInfo `json:"info"`
}

// Break is a detected commercial block, seconds from stream start.
type Break struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// BreaksState lifecycle: "" -> pending -> ready (skip mode) | cut (delete mode) | failed
type Recording struct {
	ID          string    `json:"id"`
	Path        string    `json:"path,omitempty"`
	Title       string    `json:"title"`
	Subtitle    string    `json:"subtitle,omitempty"`
	Description string    `json:"description,omitempty"`
	ChannelID   string    `json:"channelId"` // HDHR GuideNumber
	ChannelName string    `json:"channelName,omitempty"`
	Start       time.Time `json:"start"` // airing start (padding applied at capture time)
	End         time.Time `json:"end"`
	Status      string    `json:"status"` // scheduled|recording|done|failed|canceled
	Error       string    `json:"error,omitempty"`
	RuleID      string    `json:"ruleId,omitempty"`
	Breaks      []Break   `json:"breaks,omitempty"`
	BreaksState string    `json:"breaksState,omitempty"`
	Info        MediaInfo `json:"info"`
	Added       time.Time `json:"added"`
}

// Rule is a series pass: record every airing whose title matches.
type Rule struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	ChannelID string    `json:"channelId,omitempty"` // empty = any channel
	Keep      int       `json:"keep,omitempty"`      // 0 = keep all
	Created   time.Time `json:"created"`
}

type Channel struct {
	GuideNumber string `json:"guideNumber"`
	GuideName   string `json:"guideName"`
	URL         string `json:"url"`
	HD          bool   `json:"hd"`
}

type WatchState struct {
	Pos     float64   `json:"pos"`
	Dur     float64   `json:"dur"`
	Done    bool      `json:"done"`
	Updated time.Time `json:"updated"`
}

type Settings struct {
	MediaDirs         []string `json:"mediaDirs"`
	RecordingsDir     string   `json:"recordingsDir"`
	XMLTVURL          string   `json:"xmltvUrl,omitempty"` // http(s) URL or local file path
	HDHRIP            string   `json:"hdhrIp,omitempty"`   // empty = UDP auto-discover
	CommercialMode    string   `json:"commercialMode"`     // off|skip|delete
	ComskipPath       string   `json:"comskipPath"`        // binary; empty = $PATH lookup, fallback detector if absent
	FFmpegPath        string   `json:"ffmpegPath"`
	FFprobePath       string   `json:"ffprobePath"`
	PrePadMin         int      `json:"prePadMin"`
	PostPadMin        int      `json:"postPadMin"`
	AutoDeleteWatched bool     `json:"autoDeleteWatched"` // reap fully-watched recordings
}

func (s *Settings) Defaults() {
	if s.CommercialMode == "" {
		s.CommercialMode = "skip"
	}
	if s.FFmpegPath == "" {
		s.FFmpegPath = "ffmpeg"
	}
	if s.FFprobePath == "" {
		s.FFprobePath = "ffprobe"
	}
	if s.ComskipPath == "" {
		s.ComskipPath = "comskip"
	}
	if s.PrePadMin == 0 {
		s.PrePadMin = 1
	}
	if s.PostPadMin == 0 {
		s.PostPadMin = 2
	}
}

// ---- persistence -----------------------------------------------------------

type Data struct {
	Movies     map[string]*Movie      `json:"movies"`
	Episodes   map[string]*Episode    `json:"episodes"`
	Recordings map[string]*Recording  `json:"recordings"`
	Rules      map[string]*Rule       `json:"rules"`
	Watch      map[string]*WatchState `json:"watch"`
	Settings   Settings               `json:"settings"`
}

type DB struct {
	mu    sync.RWMutex
	path  string
	data  *Data
	timer *time.Timer
	tmu   sync.Mutex
}

func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	db := &DB{path: filepath.Join(dataDir, "helios.json"), data: &Data{}}
	if b, err := os.ReadFile(db.path); err == nil {
		if err := json.Unmarshal(b, db.data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", db.path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	d := db.data
	if d.Movies == nil {
		d.Movies = map[string]*Movie{}
	}
	if d.Episodes == nil {
		d.Episodes = map[string]*Episode{}
	}
	if d.Recordings == nil {
		d.Recordings = map[string]*Recording{}
	}
	if d.Rules == nil {
		d.Rules = map[string]*Rule{}
	}
	if d.Watch == nil {
		d.Watch = map[string]*WatchState{}
	}
	d.Settings.Defaults()
	return db, nil
}

// Read runs fn under the read lock. Do not retain pointers past fn.
func (db *DB) Read(fn func(*Data)) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	fn(db.data)
}

// Write runs fn under the write lock and schedules a debounced flush.
func (db *DB) Write(fn func(*Data)) {
	db.mu.Lock()
	fn(db.data)
	db.mu.Unlock()
	db.scheduleFlush()
}

func (db *DB) Settings() Settings {
	var s Settings
	db.Read(func(d *Data) { s = d.Settings })
	return s
}

func (db *DB) scheduleFlush() {
	db.tmu.Lock()
	defer db.tmu.Unlock()
	if db.timer != nil {
		db.timer.Stop()
	}
	db.timer = time.AfterFunc(2*time.Second, func() {
		if err := db.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "store: flush: %v\n", err)
		}
	})
}

// Flush writes the store atomically (tmp file + rename on the same fs).
func (db *DB) Flush() error {
	db.mu.RLock()
	b, err := json.MarshalIndent(db.data, "", " ")
	db.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := db.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, db.path)
}
