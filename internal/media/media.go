// Package media scans library directories, classifies files as movies or
// episodes from their names, probes them with ffprobe, and lazily renders
// poster/backdrop JPEGs (sidecar art wins over frame grabs).
package media

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/markchristopherwest/helios/internal/store"
)

var videoExt = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true,
	".avi": true, ".ts": true, ".m2ts": true, ".webm": true,
}

var (
	// "Show Name S01E02 Optional Title.mkv" (dots/underscores/dashes tolerated)
	reEpisode = regexp.MustCompile(`(?i)^(?P<show>.+?)[ ._-]+S(?P<s>\d{1,2})[ ._]?E(?P<e>\d{1,3})(?:[ ._-]+(?P<title>.+))?$`)
	reYear    = regexp.MustCompile(`[(\[. _](19|20)\d{2}[)\]. _]?`)
	reJunk    = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|x264|x265|h264|h265|hevc|web[- .]?dl|webrip|bluray|remux|hdtv|dvdrip|proper|repack|amzn|nf|ddp?5[. ]1|aac|atmos|hdr10?|dv)\b.*$`)
	reClean   = regexp.MustCompile(`[._]+`)
)

func ID(path string) string {
	h := sha1.Sum([]byte(path))
	return hex.EncodeToString(h[:])[:12]
}

func cleanName(s string) string {
	s = reClean.ReplaceAllString(s, " ")
	s = reJunk.ReplaceAllString(s, "")
	return strings.TrimSpace(strings.Trim(s, " -"))
}

// ParseName classifies a base filename (no extension).
func ParseName(base string) (isEpisode bool, show string, season, episode int, title string, year int) {
	if m := reEpisode.FindStringSubmatch(base); m != nil {
		show = cleanName(m[1])
		season, _ = strconv.Atoi(m[2])
		episode, _ = strconv.Atoi(m[3])
		title = cleanName(m[4])
		if ym := reYear.FindString(show); ym != "" {
			year, _ = strconv.Atoi(strings.Trim(ym, "([. _)]"))
			show = cleanName(strings.Replace(show, ym, " ", 1))
		}
		return true, show, season, episode, title, year
	}
	title = base
	if loc := reYear.FindStringIndex(base); loc != nil {
		y := strings.Trim(base[loc[0]:loc[1]], "([. _)]")
		year, _ = strconv.Atoi(y)
		title = base[:loc[0]]
	}
	return false, "", 0, 0, cleanName(title), year
}

// ---- ffprobe ----------------------------------------------------------------

type probeOut struct {
	Format struct {
		Duration   string `json:"duration"`
		FormatName string `json:"format_name"`
		Size       string `json:"size"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
}

func Probe(ctx context.Context, ffprobe, path string) (store.MediaInfo, error) {
	var info store.MediaInfo
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "quiet",
		"-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		return info, fmt.Errorf("ffprobe %s: %w", filepath.Base(path), err)
	}
	var p probeOut
	if err := json.Unmarshal(out, &p); err != nil {
		return info, err
	}
	info.DurationSec, _ = strconv.ParseFloat(p.Format.Duration, 64)
	info.SizeBytes, _ = strconv.ParseInt(p.Format.Size, 10, 64)
	// format_name can be "matroska,webm" etc: keep the first token
	info.Container = strings.Split(p.Format.FormatName, ",")[0]
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if info.VCodec == "" {
				info.VCodec, info.Width, info.Height = s.CodecName, s.Width, s.Height
			}
		case "audio":
			if info.ACodec == "" {
				info.ACodec = s.CodecName
			}
		}
	}
	return info, nil
}

// ---- scanner ----------------------------------------------------------------

type Scanner struct {
	DB       *store.DB
	scanning sync.Mutex
}

// Scan walks Settings.MediaDirs, adds new files, drops vanished ones.
// Safe to call repeatedly; concurrent calls serialize.
func (sc *Scanner) Scan(ctx context.Context) error {
	sc.scanning.Lock()
	defer sc.scanning.Unlock()
	set := sc.DB.Settings()

	seen := map[string]bool{}
	type found struct{ path string }
	var files []found
	for _, dir := range set.MediaDirs {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint: keep walking past unreadable entries
			}
			if videoExt[strings.ToLower(filepath.Ext(p))] {
				files = append(files, found{p})
			}
			return nil
		})
	}

	for _, f := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id := ID(f.path)
		seen[id] = true
		exists := false
		sc.DB.Read(func(d *store.Data) {
			_, m := d.Movies[id]
			_, e := d.Episodes[id]
			exists = m || e
		})
		if exists {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		info, err := Probe(pctx, set.FFprobePath, f.path)
		cancel()
		if err != nil {
			log.Printf("media: skip %s: %v", f.path, err)
			continue
		}
		base := strings.TrimSuffix(filepath.Base(f.path), filepath.Ext(f.path))
		isEp, show, season, ep, title, year := ParseName(base)
		now := time.Now()
		sc.DB.Write(func(d *store.Data) {
			if isEp {
				d.Episodes[id] = &store.Episode{ID: id, Path: f.path, Show: show,
					Season: season, Episode: ep, Title: title, Added: now, Info: info}
			} else {
				d.Movies[id] = &store.Movie{ID: id, Path: f.path, Title: title,
					Year: year, Added: now, Info: info}
			}
		})
		log.Printf("media: added %s", base)
	}

	// prune vanished files
	sc.DB.Write(func(d *store.Data) {
		for id, m := range d.Movies {
			if !seen[id] {
				if _, err := os.Stat(m.Path); os.IsNotExist(err) {
					delete(d.Movies, id)
				}
			}
		}
		for id, e := range d.Episodes {
			if !seen[id] {
				if _, err := os.Stat(e.Path); os.IsNotExist(err) {
					delete(d.Episodes, id)
				}
			}
		}
	})
	return nil
}

// ---- artwork ----------------------------------------------------------------

type Art struct {
	DB       *store.DB
	CacheDir string
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

func NewArt(db *store.DB, cacheDir string) *Art {
	_ = os.MkdirAll(cacheDir, 0o755)
	return &Art{DB: db, CacheDir: cacheDir, inflight: map[string]chan struct{}{}}
}

// Path returns a cached JPEG for the item, generating it on first request.
// kind: "poster" (480w, at 20%) | "backdrop" (1280w, at 40%).
func (a *Art) Path(ctx context.Context, id, kind string) (string, error) {
	out := filepath.Join(a.CacheDir, id+"-"+kind+".jpg")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	// collapse concurrent generation of the same image
	a.mu.Lock()
	if ch, busy := a.inflight[out]; busy {
		a.mu.Unlock()
		<-ch
		if _, err := os.Stat(out); err == nil {
			return out, nil
		}
		return "", fmt.Errorf("art generation failed for %s", id)
	}
	ch := make(chan struct{})
	a.inflight[out] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.inflight, out)
		a.mu.Unlock()
		close(ch)
	}()

	src, dur := a.lookup(id)
	if src == "" {
		return "", fmt.Errorf("unknown item %s", id)
	}
	// sidecar art beats frame grabs
	dir := filepath.Dir(src)
	side := map[string][]string{
		"poster":   {"poster.jpg", "poster.png", "folder.jpg", "cover.jpg"},
		"backdrop": {"fanart.jpg", "backdrop.jpg", "fanart.png"},
	}
	for _, name := range side[kind] {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return a.resize(ctx, filepath.Join(dir, name), out, kind)
		}
	}
	at, width := dur*0.2, "480"
	if kind == "backdrop" {
		at, width = dur*0.4, "1280"
	}
	if at <= 0 {
		at = 60
	}
	set := a.DB.Settings()
	cmd := exec.CommandContext(ctx, set.FFmpegPath, "-hide_banner", "-loglevel", "error",
		"-ss", fmt.Sprintf("%.1f", at), "-i", src, "-frames:v", "1",
		"-vf", "scale="+width+":-2", "-q:v", "3", "-y", out)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("frame grab: %w", err)
	}
	return out, nil
}

func (a *Art) resize(ctx context.Context, in, out, kind string) (string, error) {
	w := "480"
	if kind == "backdrop" {
		w = "1280"
	}
	set := a.DB.Settings()
	err := exec.CommandContext(ctx, set.FFmpegPath, "-hide_banner", "-loglevel", "error",
		"-i", in, "-vf", "scale="+w+":-2", "-q:v", "3", "-y", out).Run()
	return out, err
}

func (a *Art) lookup(id string) (path string, dur float64) {
	a.DB.Read(func(d *store.Data) {
		if m, ok := d.Movies[id]; ok {
			path, dur = m.Path, m.Info.DurationSec
		} else if e, ok := d.Episodes[id]; ok {
			path, dur = e.Path, e.Info.DurationSec
		} else if r, ok := d.Recordings[id]; ok {
			path, dur = r.Path, r.Info.DurationSec
		}
	})
	return
}
