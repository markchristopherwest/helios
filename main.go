// helios — a single-binary, zero-Go-dependency media server:
// library scanning + playback (direct or HLS transcode), HDHomeRun live TV,
// DVR with series rules, and commercial skip/removal (comskip or built-in
// fallback). External runtime deps: ffmpeg/ffprobe (required), comskip
// (recommended). Flags bootstrap Settings on first run; thereafter edit via
// the UI or PUT /api/settings — the JSON store is the source of truth.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/markchristopherwest/helios/internal/api"
	"github.com/markchristopherwest/helios/internal/dvr"
	"github.com/markchristopherwest/helios/internal/epg"
	"github.com/markchristopherwest/helios/internal/hdhr"
	"github.com/markchristopherwest/helios/internal/media"
	"github.com/markchristopherwest/helios/internal/store"
	"github.com/markchristopherwest/helios/internal/stream"
	"github.com/markchristopherwest/helios/web"
)

func main() {
	var (
		addr       = flag.String("addr", ":7979", "listen address")
		dataDir    = flag.String("data", "./helios-data", "state + cache directory")
		mediaDirs  = flag.String("media", "", "comma-separated library dirs (first-run bootstrap)")
		recDir     = flag.String("recordings", "", "DVR output dir (first-run bootstrap)")
		xmltv      = flag.String("xmltv", "", "XMLTV guide URL or file (first-run bootstrap)")
		hdhrIP     = flag.String("hdhr", "", "HDHomeRun IP (first-run bootstrap; empty = auto-discover)")
		commercial = flag.String("commercials", "", "off|skip|delete (first-run bootstrap)")
	)
	flag.Parse()

	db, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// Flags seed settings only where unset, so a redeploy with different
	// flags never clobbers what was tuned in the UI.
	db.Write(func(d *store.Data) {
		s := &d.Settings
		if len(s.MediaDirs) == 0 && *mediaDirs != "" {
			for _, p := range strings.Split(*mediaDirs, ",") {
				if p = strings.TrimSpace(p); p != "" {
					s.MediaDirs = append(s.MediaDirs, p)
				}
			}
		}
		if s.RecordingsDir == "" {
			if *recDir != "" {
				s.RecordingsDir = *recDir
			} else {
				s.RecordingsDir = filepath.Join(*dataDir, "recordings")
			}
		}
		if s.XMLTVURL == "" {
			s.XMLTVURL = *xmltv
		}
		if s.HDHRIP == "" {
			s.HDHRIP = *hdhrIP
		}
		if *commercial != "" {
			s.CommercialMode = *commercial
		}
		s.Defaults()
	})
	set := db.Settings()
	_ = os.MkdirAll(set.RecordingsDir, 0o755)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	scanner := &media.Scanner{DB: db}
	art := media.NewArt(db, filepath.Join(*dataDir, "img"))
	streams := stream.NewManager(db, filepath.Join(*dataDir, "hls"))
	tuner := &hdhr.Client{DB: db}
	guide := &epg.Guide{DB: db}
	engine := dvr.New(db, guide, tuner)

	// background loops
	go streams.GC(ctx)
	go engine.Run(ctx)
	go func() { // initial + periodic library scan
		for {
			if err := scanner.Scan(ctx); err != nil && ctx.Err() == nil {
				log.Printf("scan: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Minute):
			}
		}
	}()
	go func() { // tuner + guide refresh
		for {
			rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
			if err := tuner.Refresh(rctx); err != nil {
				log.Printf("hdhr: %v", err)
			}
			rcancel()
			gctx, gcancel := context.WithTimeout(ctx, 5*time.Minute)
			if err := guide.Refresh(gctx, tuner.Channels()); err != nil {
				log.Printf("epg: %v", err)
			}
			gcancel()
			select {
			case <-ctx.Done():
				return
			case <-time.After(4 * time.Hour):
			}
		}
	}()

	srv := &http.Server{
		Addr: *addr,
		Handler: (&api.Server{
			DB: db, Scanner: scanner, Art: art, HDHR: tuner,
			Guide: guide, DVR: engine, Streams: streams, UI: web.UI(),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shCancel()
		_ = srv.Shutdown(shCtx)
	}()

	log.Printf("helios listening on %s (data: %s)", *addr, *dataDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	if err := db.Flush(); err != nil {
		log.Printf("store: final flush: %v", err)
	}
}
