// Package api wires the REST surface and serves the embedded SPA.
// Go 1.22 pattern mux (method + {wildcards}), JSON in/out, no framework.
// No auth by design: helios is a LAN service — put it behind your Gateway
// API / oauth2-proxy if you expose it. See README "Exposure".
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/markchristopherwest/helios/internal/dvr"
	"github.com/markchristopherwest/helios/internal/epg"
	"github.com/markchristopherwest/helios/internal/hdhr"
	"github.com/markchristopherwest/helios/internal/media"
	"github.com/markchristopherwest/helios/internal/store"
	"github.com/markchristopherwest/helios/internal/stream"
)

type Server struct {
	DB      *store.DB
	Scanner *media.Scanner
	Art     *media.Art
	HDHR    *hdhr.Client
	Guide   *epg.Guide
	DVR     *dvr.Engine
	Streams *stream.Manager
	UI      fs.FS
}

// item is the unified shape the frontend renders everywhere.
type item struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"` // movie|episode|recording
	Title       string            `json:"title"`
	Subtitle    string            `json:"subtitle,omitempty"`
	Show        string            `json:"show,omitempty"`
	Season      int               `json:"season,omitempty"`
	Episode     int               `json:"episode,omitempty"`
	Year        int               `json:"year,omitempty"`
	Duration    float64           `json:"duration"`
	VCodec      string            `json:"vcodec,omitempty"`
	ACodec      string            `json:"acodec,omitempty"`
	Height      int               `json:"height,omitempty"`
	Container   string            `json:"container,omitempty"`
	Added       time.Time         `json:"added"`
	Watch       *store.WatchState `json:"watch,omitempty"`
	Breaks      []store.Break     `json:"breaks,omitempty"`
	BreaksState string            `json:"breaksState,omitempty"`
	Status      string            `json:"status,omitempty"`
	Channel     string            `json:"channel,omitempty"`
	Start       *time.Time        `json:"start,omitempty"`
	End         *time.Time        `json:"end,omitempty"`
}

func movieItem(m *store.Movie, w *store.WatchState) item {
	return item{ID: m.ID, Kind: "movie", Title: m.Title, Year: m.Year,
		Duration: m.Info.DurationSec, VCodec: m.Info.VCodec, ACodec: m.Info.ACodec,
		Height: m.Info.Height, Container: m.Info.Container, Added: m.Added, Watch: w}
}
func episodeItem(e *store.Episode, w *store.WatchState) item {
	return item{ID: e.ID, Kind: "episode", Title: e.Title, Show: e.Show,
		Season: e.Season, Episode: e.Episode,
		Duration: e.Info.DurationSec, VCodec: e.Info.VCodec, ACodec: e.Info.ACodec,
		Height: e.Info.Height, Container: e.Info.Container, Added: e.Added, Watch: w}
}
func recordingItem(r *store.Recording, w *store.WatchState) item {
	s, e := r.Start, r.End
	return item{ID: r.ID, Kind: "recording", Title: r.Title, Subtitle: r.Subtitle,
		Duration: r.Info.DurationSec, VCodec: r.Info.VCodec, ACodec: r.Info.ACodec,
		Height: r.Info.Height, Container: r.Info.Container, Added: r.Added, Watch: w,
		Breaks: r.Breaks, BreaksState: r.BreaksState, Status: r.Status,
		Channel: r.ChannelName, Start: &s, End: &e}
}

func (s *Server) findItem(id string) (it item, path string, ok bool) {
	s.DB.Read(func(d *store.Data) {
		w := d.Watch[id]
		if m, found := d.Movies[id]; found {
			it, path, ok = movieItem(m, w), m.Path, true
		} else if e, found := d.Episodes[id]; found {
			it, path, ok = episodeItem(e, w), e.Path, true
		} else if r, found := d.Recordings[id]; found {
			it, path, ok = recordingItem(r, w), r.Path, true
		}
	})
	return
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---- library ----
	mux.HandleFunc("GET /api/home", s.handleHome)
	mux.HandleFunc("GET /api/movies", s.handleMovies)
	mux.HandleFunc("GET /api/shows", s.handleShows)
	mux.HandleFunc("GET /api/shows/{show}", s.handleShow)
	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		it, _, ok := s.findItem(r.PathValue("id"))
		if !ok {
			httpErr(w, 404, "unknown item")
			return
		}
		writeJSON(w, it)
	})
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("POST /api/scan", func(w http.ResponseWriter, r *http.Request) {
		go func() {
			if err := s.Scanner.Scan(context.Background()); err != nil {
				log.Printf("scan: %v", err)
			}
		}()
		writeJSON(w, map[string]string{"status": "scanning"})
	})
	mux.HandleFunc("GET /api/img/{id}", s.handleImg)

	// ---- watch state ----
	mux.HandleFunc("POST /api/watch/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Pos, Dur float64
			Done     bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		id := r.PathValue("id")
		done := body.Done || (body.Dur > 0 && body.Pos/body.Dur > 0.95)
		s.DB.Write(func(d *store.Data) {
			d.Watch[id] = &store.WatchState{Pos: body.Pos, Dur: body.Dur, Done: done, Updated: time.Now()}
		})
		writeJSON(w, map[string]bool{"ok": true})
	})

	// ---- live tv + guide ----
	mux.HandleFunc("GET /api/livetv/channels", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"device": s.HDHR.Device(), "channels": s.HDHR.Channels()})
	})
	mux.HandleFunc("POST /api/livetv/refresh", func(w http.ResponseWriter, r *http.Request) {
		if err := s.HDHR.Refresh(r.Context()); err != nil {
			httpErr(w, 502, err.Error())
			return
		}
		go s.Guide.Refresh(context.Background(), s.HDHR.Channels()) //nolint:errcheck
		writeJSON(w, map[string]any{"channels": s.HDHR.Channels()})
	})
	mux.HandleFunc("GET /api/guide", func(w http.ResponseWriter, r *http.Request) {
		hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
		if hours <= 0 || hours > 48 {
			hours = 6
		}
		now := time.Now()
		writeJSON(w, map[string]any{
			"updated": s.Guide.Updated(),
			"from":    now, "to": now.Add(time.Duration(hours) * time.Hour),
			"airings": s.Guide.Window(now.Add(-30*time.Minute), now.Add(time.Duration(hours)*time.Hour)),
		})
	})

	// ---- dvr ----
	mux.HandleFunc("GET /api/dvr/recordings", s.handleRecordings)
	mux.HandleFunc("POST /api/dvr/record", s.handleManualRecord)
	mux.HandleFunc("DELETE /api/dvr/recordings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s.DVR.Cancel(id)
		var path string
		s.DB.Write(func(d *store.Data) {
			if rec, ok := d.Recordings[id]; ok {
				path = rec.Path
				delete(d.Recordings, id)
				delete(d.Watch, id)
			}
		})
		if path != "" {
			_ = removeFile(path)
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/dvr/recordings/{id}/adscan", func(w http.ResponseWriter, r *http.Request) {
		cut := r.URL.Query().Get("cut") == "1"
		if err := s.DVR.RescanCommercials(r.Context(), r.PathValue("id"), cut); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		it, _, _ := s.findItem(r.PathValue("id"))
		writeJSON(w, it)
	})
	mux.HandleFunc("GET /api/dvr/rules", func(w http.ResponseWriter, r *http.Request) {
		var rules []store.Rule
		s.DB.Read(func(d *store.Data) {
			for _, ru := range d.Rules {
				rules = append(rules, *ru)
			}
		})
		sort.Slice(rules, func(i, j int) bool { return rules[i].Created.After(rules[j].Created) })
		writeJSON(w, rules)
	})
	mux.HandleFunc("POST /api/dvr/rules", func(w http.ResponseWriter, r *http.Request) {
		var body store.Rule
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Title) == "" {
			httpErr(w, 400, "need title")
			return
		}
		body.ID, body.Created = newRandID(), time.Now()
		s.DB.Write(func(d *store.Data) { d.Rules[body.ID] = &body })
		writeJSON(w, body)
	})
	mux.HandleFunc("DELETE /api/dvr/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.DB.Write(func(d *store.Data) { delete(d.Rules, r.PathValue("id")) })
		writeJSON(w, map[string]bool{"ok": true})
	})

	// ---- settings ----
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.DB.Settings())
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var set store.Settings
		if err := json.NewDecoder(r.Body).Decode(&set); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		set.Defaults()
		s.DB.Write(func(d *store.Data) { d.Settings = set })
		writeJSON(w, set)
	})

	// ---- playback ----
	mux.HandleFunc("GET /stream/direct/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, path, ok := s.findItem(r.PathValue("id"))
		if !ok || path == "" {
			httpErr(w, 404, "unknown item")
			return
		}
		stream.DirectPlay(w, r, path)
	})
	mux.HandleFunc("POST /api/stream/start", s.handleStreamStart)
	mux.HandleFunc("DELETE /api/stream/{sid}", func(w http.ResponseWriter, r *http.Request) {
		s.Streams.Stop(r.PathValue("sid"))
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /stream/hls/{sid}/{file}", func(w http.ResponseWriter, r *http.Request) {
		s.Streams.ServeSegment(w, r, r.PathValue("sid"), r.PathValue("file"))
	})

	// ---- SPA ----
	// http.FileServer 301-loops on any path resolving to index.html, so the
	// shell is written straight from the embedded FS; assets go through the
	// FileServer for range/caching behavior.
	fileServer := http.FileServer(http.FS(s.UI))
	index, err := fs.ReadFile(s.UI, "index.html")
	if err != nil {
		panic("embedded UI missing index.html: " + err.Error())
	}
	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || p == "index.html" {
			serveIndex(w)
			return
		}
		if _, err := fs.Stat(s.UI, p); err != nil {
			serveIndex(w) // hash router: unknown paths land on the app shell
			return
		}
		fileServer.ServeHTTP(w, r)
	})
	return mux
}

// ---- handlers ---------------------------------------------------------------

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	var cont, movies, episodes, recs []item
	s.DB.Read(func(d *store.Data) {
		for id, ws := range d.Watch {
			if ws.Done || ws.Pos < 60 {
				continue
			}
			if m, ok := d.Movies[id]; ok {
				cont = append(cont, movieItem(m, ws))
			} else if e, ok := d.Episodes[id]; ok {
				cont = append(cont, episodeItem(e, ws))
			} else if rc, ok := d.Recordings[id]; ok {
				cont = append(cont, recordingItem(rc, ws))
			}
		}
		for _, m := range d.Movies {
			movies = append(movies, movieItem(m, d.Watch[m.ID]))
		}
		for _, e := range d.Episodes {
			episodes = append(episodes, episodeItem(e, d.Watch[e.ID]))
		}
		for _, rc := range d.Recordings {
			if rc.Status == "done" {
				recs = append(recs, recordingItem(rc, d.Watch[rc.ID]))
			}
		}
	})
	byUpdated := func(list []item) {
		sort.Slice(list, func(i, j int) bool {
			wi, wj := list[i].Watch, list[j].Watch
			if wi != nil && wj != nil {
				return wi.Updated.After(wj.Updated)
			}
			return wi != nil
		})
	}
	byAdded := func(list []item) {
		sort.Slice(list, func(i, j int) bool { return list[i].Added.After(list[j].Added) })
	}
	byUpdated(cont)
	byAdded(movies)
	byAdded(episodes)
	byAdded(recs)
	writeJSON(w, map[string][]item{
		"continue": cap20(cont), "movies": cap20(movies),
		"episodes": cap20(episodes), "recordings": cap20(recs),
	})
}

func cap20(l []item) []item {
	if len(l) > 20 {
		return l[:20]
	}
	if l == nil {
		return []item{}
	}
	return l
}

func (s *Server) handleMovies(w http.ResponseWriter, r *http.Request) {
	var out []item
	s.DB.Read(func(d *store.Data) {
		for _, m := range d.Movies {
			out = append(out, movieItem(m, d.Watch[m.ID]))
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	writeJSON(w, out)
}

type showSummary struct {
	Show     string `json:"show"`
	Episodes int    `json:"episodes"`
	Seasons  int    `json:"seasons"`
	PosterID string `json:"posterId"`
}

func (s *Server) handleShows(w http.ResponseWriter, r *http.Request) {
	type agg struct {
		count   int
		seasons map[int]bool
		poster  string
	}
	m := map[string]*agg{}
	s.DB.Read(func(d *store.Data) {
		for _, e := range d.Episodes {
			a := m[e.Show]
			if a == nil {
				a = &agg{seasons: map[int]bool{}, poster: e.ID}
				m[e.Show] = a
			}
			a.count++
			a.seasons[e.Season] = true
		}
	})
	out := make([]showSummary, 0, len(m))
	for show, a := range m {
		out = append(out, showSummary{Show: show, Episodes: a.count, Seasons: len(a.seasons), PosterID: a.poster})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Show < out[j].Show })
	writeJSON(w, out)
}

func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	show := r.PathValue("show")
	var eps []item
	s.DB.Read(func(d *store.Data) {
		for _, e := range d.Episodes {
			if strings.EqualFold(e.Show, show) {
				eps = append(eps, episodeItem(e, d.Watch[e.ID]))
			}
		}
	})
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Season != eps[j].Season {
			return eps[i].Season < eps[j].Season
		}
		return eps[i].Episode < eps[j].Episode
	})
	writeJSON(w, map[string]any{"show": show, "episodes": eps})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	var out []item
	if q == "" {
		writeJSON(w, out)
		return
	}
	match := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(strings.ToLower(p), q) {
				return true
			}
		}
		return false
	}
	s.DB.Read(func(d *store.Data) {
		for _, m := range d.Movies {
			if match(m.Title) {
				out = append(out, movieItem(m, d.Watch[m.ID]))
			}
		}
		for _, e := range d.Episodes {
			if match(e.Show, e.Title) {
				out = append(out, episodeItem(e, d.Watch[e.ID]))
			}
		}
		for _, rc := range d.Recordings {
			if rc.Status == "done" && match(rc.Title, rc.Subtitle) {
				out = append(out, recordingItem(rc, d.Watch[rc.ID]))
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	if len(out) > 50 {
		out = out[:50]
	}
	writeJSON(w, out)
}

func (s *Server) handleImg(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("type")
	if kind != "backdrop" {
		kind = "poster"
	}
	p, err := s.Art.Path(r.Context(), r.PathValue("id"), kind)
	if err != nil {
		httpErr(w, 404, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, p)
}

func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	var out []item
	s.DB.Read(func(d *store.Data) {
		for _, rc := range d.Recordings {
			out = append(out, recordingItem(rc, d.Watch[rc.ID]))
		}
	})
	sort.Slice(out, func(i, j int) bool {
		si, sj := out[i].Start, out[j].Start
		return si != nil && sj != nil && si.After(*sj)
	})
	writeJSON(w, out)
}

func (s *Server) handleManualRecord(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChannelID string    `json:"channelId"`
		Title     string    `json:"title"`
		Subtitle  string    `json:"subtitle"`
		Start     time.Time `json:"start"`
		End       time.Time `json:"end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if body.ChannelID == "" || !body.End.After(body.Start) {
		httpErr(w, 400, "need channelId and start < end")
		return
	}
	if body.Title == "" {
		body.Title = "Manual recording"
	}
	rec := &store.Recording{
		ID: newRandID(), Title: body.Title, Subtitle: body.Subtitle,
		ChannelID: body.ChannelID, Start: body.Start, End: body.End,
		Status: "scheduled", Added: time.Now(),
	}
	s.DB.Write(func(d *store.Data) { d.Recordings[rec.ID] = rec })
	writeJSON(w, recordingItem(rec, nil))
}

func (s *Server) handleStreamStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string  `json:"id"`      // library/recording item…
		Channel string  `json:"channel"` // …or live GuideNumber
		Start   float64 `json:"start"`
		Quality string  `json:"quality"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if body.Quality == "" {
		body.Quality = "720"
	}
	var input string
	live := false
	if body.Channel != "" {
		input = s.HDHR.StreamURL(body.Channel)
		live = true
		if input == "" {
			httpErr(w, 502, "channel unavailable — refresh the tuner lineup")
			return
		}
	} else {
		_, path, ok := s.findItem(body.ID)
		if !ok || path == "" {
			httpErr(w, 404, "unknown item")
			return
		}
		input = path
	}
	sess, err := s.Streams.Start(r.Context(), input, body.Start, body.Quality, live)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"sessionId": sess.ID,
		"url":       "/stream/hls/" + sess.ID + "/index.m3u8",
		"offset":    sess.StartSec,
		"live":      live,
	})
}

func newRandID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func removeFile(p string) error {
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
