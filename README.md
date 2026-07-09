# Helios

Single-binary media server in Go: library streaming, HDHomeRun live TV, DVR
with series passes, and commercial skip **and** deletion. Zero Go dependencies
— the entire web UI (custom player, vendored hls.js + fonts) is embedded, so
`go build` produces the whole product (~10 MB).

## Runtime dependencies

| Binary | Required | Used for |
|---|---|---|
| `ffmpeg` / `ffprobe` | yes | probing, HLS transcode, remux, thumbnails, cutting |
| `comskip` | recommended | commercial detection. Without it Helios falls back to a built-in black-frame ∩ silence heuristic (works, less accurate). |

## Quick start

```sh
go build -o helios .
./helios \
  -media /tank/movies,/tank/tv \
  -recordings /tank/dvr \
  -xmltv /tank/guide/xmltv.xml \
  -commercials delete        # off | skip | delete
# open http://host:7979
```

Flags only **bootstrap** settings on first run; after that the UI (Settings)
and `PUT /api/settings` own them — redeploys never clobber tuned values.
State is one JSON file under `-data` (default `./helios-data`).

Naming: movies `Title (2023).mkv`, episodes `Show S01E02 Title.mkv`
(dots/dashes tolerated, release-group junk stripped). Sidecar `poster.jpg` /
`fanart.jpg` override generated artwork.

## Live TV + DVR (HDHomeRun)

- Tuner discovery: native UDP broadcast (libhdhomerun wire format) with
  `discover.json`/`lineup.json` over HTTP; behind VLANs/k8s set the IP in
  Settings instead.
- Guide: any XMLTV source (URL or file, `.gz` ok) — zap2xml, Schedules Direct
  grabbers, etc. Channels are matched to the lineup by guide number/name.
- Series passes record every airing matching a title, with keep-N pruning,
  pre/post padding, and one-click "record this airing" from Live TV. Capture
  is a raw HTTP copy of the tuner's MPEG-TS (the HDHomeRun does its own tuner
  allocation), then a stream-copy remux to `.mkv`.

## Commercials

After each recording (mode `skip` or `delete`):

1. **Detect** — comskip with a generated ini (`output_edl=1`), EDL parsed into
   break spans; fallback detector intersects `blackdetect` + `silencedetect`
   and clusters junctions into ad pods.
2. **skip** — breaks are stored; the player shows gold markers on the
   scrubber, a *Skip break* pill, and auto-skips when enabled (default on).
3. **delete** — breaks are cut out: per-segment `-c copy` extraction +
   concat demuxer, original replaced. Keyframe-snapped (±GOP), no re-encode.
   `POST /api/dvr/recordings/{id}/adscan?cut=1` re-runs it on demand.

Optional: *delete recordings after fully watched* (Settings).

## Playback

Direct play (range requests) when the browser can decode the source
(h264 + mp4/mkv/webm), otherwise per-session ffmpeg HLS at
original/1080/720/480 with instant out-of-window seeks (new `-ss` session),
live transcode with deinterlace for broadcast MPEG-2, idle-session GC.
The player: ambient light bleed sampled from the video, auto-hiding glass
controls, ad markers, PiP, keyboard (`space ← → f m s esc`, `/` to search).

## API (used by the UI, stable enough to script against)

```
GET  /api/home | /api/movies | /api/shows | /api/shows/{show} | /api/items/{id}
GET  /api/search?q= | /api/img/{id}?type=poster|backdrop
POST /api/scan | /api/watch/{id} {pos,dur}
GET  /api/livetv/channels | /api/guide?hours=N     POST /api/livetv/refresh
GET  /api/dvr/recordings | /api/dvr/rules
POST /api/dvr/record {channelId,title,start,end}   POST /api/dvr/rules {title,channelId?,keep}
POST /api/dvr/recordings/{id}/adscan[?cut=1]
DEL  /api/dvr/recordings/{id} | /api/dvr/rules/{id}
POST /api/stream/start {id|channel,start,quality} → {url,sessionId}   DEL /api/stream/{sid}
GET  /stream/direct/{id} | /stream/hls/{sid}/{file}
GET/PUT /api/settings
```

## Container / Kubernetes

`Dockerfile` builds Helios + Comskip from source on `bookworm-slim` with
ffmpeg (amd64/arm64). Notes for k8s:

- UDP broadcast discovery won't cross the CNI boundary — either
  `hostNetwork: true` or pin the tuner IP in settings (one env-free flag:
  `-hdhr 192.168.1.50`).
- Give `/data`, `/recordings` PVCs; media can be RO.
- Transcode wants CPU: request ~2 cores burst for 1080p veryfast on a NUC;
  pin the Deployment to amd64 unless you're happy with Pi-speed x264.

## Scope (honest)

This is the working core of a Plex-shaped server, not a Plex clone: no auth /
multi-user (front it with oauth2-proxy or your Gateway), no external metadata
agents (artwork comes from frame grabs + sidecars; a TMDB fetcher slots
naturally into `internal/media`), no hardware transcode (add
`-c:v h264_vaapi/_qsv` in `internal/stream`), subtitles pass through only on
direct play. Verified: `go build`, `go vet`, `go test`, plus an end-to-end
smoke (scan → poster → direct range → HLS session → ad detect → hard cut).

## License notes

Vendored: hls.js (Apache-2.0, `web/static/vendor/hls.LICENSE.txt`),
Sora + Inter fonts (OFL, `web/static/vendor/fonts/OFL.txt`).
Comskip (GPL-2.0) is invoked as an external process.
