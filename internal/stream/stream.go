// Package stream owns playback: direct play (range-served original file when
// the browser can decode it) and on-the-fly HLS transcodes via ffmpeg for
// everything else, including live MPEG-2 off the HDHomeRun. One ffmpeg per
// session writing 4s segments into a scratch dir; sessions idle >90s are
// killed and swept. Seeking outside the transcoded range = new session with
// -ss (the UI offsets its timeline by the session start).
package stream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/markchristopherwest/helios/internal/store"
)

type Session struct {
	ID       string
	Dir      string
	StartSec float64
	Live     bool
	cmd      *exec.Cmd
	cancel   context.CancelFunc

	mu         sync.Mutex
	lastAccess time.Time
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

type Manager struct {
	DB       *store.DB
	CacheDir string

	mu       sync.Mutex
	sessions map[string]*Session
}

func NewManager(db *store.DB, cacheDir string) *Manager {
	_ = os.MkdirAll(cacheDir, 0o755)
	return &Manager{DB: db, CacheDir: cacheDir, sessions: map[string]*Session{}}
}

func randID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start launches a transcode. input is a file path or a live http URL.
// quality: original|1080|720|480 ("original" = video stream copy — only offer
// it when the source codec is browser-decodable).
func (m *Manager) Start(ctx context.Context, input string, startSec float64, quality string, live bool) (*Session, error) {
	set := m.DB.Settings()
	id := randID()
	dir := filepath.Join(m.CacheDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-nostats"}
	if live {
		// broadcast TS: give the demuxer room to find streams
		args = append(args, "-analyzeduration", "3M", "-probesize", "8M")
	}
	if startSec > 0 && !live {
		args = append(args, "-ss", fmt.Sprintf("%.3f", startSec))
	}
	args = append(args, "-i", input, "-map", "0:v:0", "-map", "0:a:0?")

	switch quality {
	case "original":
		args = append(args, "-c:v", "copy")
	default:
		h, maxrate := "1080", "8M"
		switch quality {
		case "720":
			h, maxrate = "720", "4M"
		case "480":
			h, maxrate = "480", "2M"
		}
		vf := "scale=-2:" + h
		if live {
			vf = "yadif," + vf // broadcast is interlaced more often than not
		}
		args = append(args,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "21",
			"-maxrate", maxrate, "-bufsize", maxrate,
			"-vf", vf,
			// keyframe every segment so hls.js seeks cleanly
			"-force_key_frames", "expr:gte(t,n_forced*4)",
		)
	}
	args = append(args,
		"-c:a", "aac", "-ac", "2", "-b:a", "160k",
		"-f", "hls", "-hls_time", "4", "-hls_playlist_type", "event",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_filename", filepath.Join(dir, "s%05d.ts"),
		filepath.Join(dir, "index.m3u8"),
	)

	sctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(sctx, set.FFmpegPath, args...)
	if err := cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, err
	}
	s := &Session{ID: id, Dir: dir, StartSec: startSec, Live: live,
		cmd: cmd, cancel: cancel, lastAccess: time.Now()}
	go func() { _ = cmd.Wait() }() // reap; GC handles cleanup

	// don't hand the playlist URL back until it exists
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "index.m3u8")); err == nil {
			m.mu.Lock()
			m.sessions[id] = s
			m.mu.Unlock()
			return s, nil
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	cancel()
	os.RemoveAll(dir)
	return nil, fmt.Errorf("transcode failed to start (check ffmpeg + input)")
}

func (m *Manager) Stop(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.cancel()
		go func() { time.Sleep(time.Second); os.RemoveAll(s.Dir) }()
	}
}

// ServeSegment handles GET /stream/hls/{sid}/{file}.
func (m *Manager) ServeSegment(w http.ResponseWriter, r *http.Request, sid, file string) {
	m.mu.Lock()
	s := m.sessions[sid]
	m.mu.Unlock()
	if s == nil {
		http.Error(w, "session expired; restart playback", http.StatusGone)
		return
	}
	s.touch()
	// no traversal, no subdirs
	if file != filepath.Base(file) || strings.Contains(file, "..") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(file, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	http.ServeFile(w, r, filepath.Join(s.Dir, file))
}

// GC kills idle sessions; run it as a goroutine.
func (m *Manager) GC(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for id, s := range m.sessions {
				s.cancel()
				os.RemoveAll(s.Dir)
				delete(m.sessions, id)
			}
			m.mu.Unlock()
			return
		case <-t.C:
			m.mu.Lock()
			for id, s := range m.sessions {
				s.mu.Lock()
				idle := time.Since(s.lastAccess)
				s.mu.Unlock()
				if idle > 90*time.Second {
					s.cancel()
					os.RemoveAll(s.Dir)
					delete(m.sessions, id)
				}
			}
			m.mu.Unlock()
		}
	}
}

// DirectPlay range-serves the original file (browser handles decode).
func DirectPlay(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file missing on disk", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctype := map[string]string{
		".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime",
		".mkv": "video/x-matroska", ".webm": "video/webm", ".ts": "video/mp2t",
	}[strings.ToLower(filepath.Ext(path))]
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}
