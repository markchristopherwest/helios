// Package commercials finds and removes ad breaks in recordings.
//
// Detection prefers Comskip (https://github.com/erikkaashoek/Comskip) — the
// battle-tested detector every DVR ecosystem leans on — invoked with a
// generated ini forcing EDL output. When comskip isn't installed we fall back
// to a pure-ffmpeg heuristic: intersect blackdetect and silencedetect events
// (broadcast ad pods are stitched with black+silent frames between spots) and
// cluster the cut points into break blocks. The fallback is honest about
// being a heuristic; install comskip for production-grade accuracy.
//
// "skip" mode stores breaks for the player to jump over; "delete" mode
// hard-cuts them out with stream-copy segment extraction + concat demuxer
// (fast, no re-encode; cuts land on the nearest keyframe, so expect ±GOP
// precision — the same tradeoff MCEBuddy/lossless-cut make).
package commercials

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/markchristopherwest/helios/internal/store"
)

// Detect returns commercial breaks for path using comskip when available.
func Detect(ctx context.Context, set store.Settings, path string) ([]store.Break, error) {
	if bin, err := exec.LookPath(set.ComskipPath); err == nil {
		return runComskip(ctx, bin, set, path)
	}
	return ffmpegFallback(ctx, set, path)
}

// ---- comskip ----------------------------------------------------------------

func runComskip(ctx context.Context, bin string, set store.Settings, path string) ([]store.Break, error) {
	work, err := os.MkdirTemp("", "helios-comskip-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	// Minimal ini: EDL is the only artifact we consume.
	ini := filepath.Join(work, "comskip.ini")
	if err := os.WriteFile(ini, []byte("output_edl=1\noutput_default=0\n"), 0o644); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "--ini="+ini, "--output="+work, path)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Run(); err != nil {
		// comskip exits 1 when it finds commercials on some builds; trust the
		// EDL if it materialized, otherwise surface the error.
		if _, statErr := os.Stat(edlPath(work, path)); statErr != nil {
			return nil, fmt.Errorf("comskip: %w", err)
		}
	}
	return parseEDL(edlPath(work, path))
}

func edlPath(dir, media string) string {
	base := strings.TrimSuffix(filepath.Base(media), filepath.Ext(media))
	return filepath.Join(dir, base+".edl")
}

// parseEDL reads "start<TAB>end<TAB>action" lines; action 0 (cut) and
// 3 (commercial break) both mark ads.
func parseEDL(path string) ([]store.Break, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []store.Break
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		start, e1 := strconv.ParseFloat(fields[0], 64)
		end, e2 := strconv.ParseFloat(fields[1], 64)
		action, e3 := strconv.Atoi(fields[2])
		if e1 != nil || e2 != nil || e3 != nil || end <= start {
			continue
		}
		if action == 0 || action == 3 {
			out = append(out, store.Break{Start: start, End: end})
		}
	}
	return out, sc.Err()
}

// ---- ffmpeg fallback ---------------------------------------------------------

var (
	reBlack   = regexp.MustCompile(`black_start:([\d.]+).*?black_end:([\d.]+)`)
	reSilence = regexp.MustCompile(`silence_end: ([\d.]+) \| silence_duration: ([\d.]+)`)
)

type span struct{ a, b float64 }

func ffmpegFallback(ctx context.Context, set store.Settings, path string) ([]store.Break, error) {
	cmd := exec.CommandContext(ctx, set.FFmpegPath, "-hide_banner", "-nostats", "-i", path,
		"-vf", "blackdetect=d=0.4:pix_th=0.10",
		"-af", "silencedetect=n=-35dB:d=0.3",
		"-f", "null", "-")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var blacks, silences []span
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := reBlack.FindStringSubmatch(line); m != nil {
			a, _ := strconv.ParseFloat(m[1], 64)
			b, _ := strconv.ParseFloat(m[2], 64)
			blacks = append(blacks, span{a, b})
		} else if m := reSilence.FindStringSubmatch(line); m != nil {
			end, _ := strconv.ParseFloat(m[1], 64)
			dur, _ := strconv.ParseFloat(m[2], 64)
			silences = append(silences, span{end - dur, end})
		}
	}
	_ = cmd.Wait() // -f null exits 0; scan errors matter more than exit code here
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// Cut point = black frame run overlapping a silence run.
	var cuts []float64
	for _, bl := range blacks {
		for _, si := range silences {
			if bl.a < si.b && si.a < bl.b {
				cuts = append(cuts, (max64(bl.a, si.a)+min64(bl.b, si.b))/2)
				break
			}
		}
	}
	// Some stations duck audio at junctions without hitting true silence;
	// long pure-black holds (>=1.2s) are a strong enough signal on their own.
	if len(cuts) < 2 {
		cuts = cuts[:0]
		for _, bl := range blacks {
			if bl.b-bl.a >= 1.2 {
				cuts = append(cuts, (bl.a+bl.b)/2)
			}
		}
	}
	sort.Float64s(cuts)

	// Ad pods read as clusters of cut points: 15/30/60s spots separated by
	// black+silence, so ≥2 cuts within 240s of each other spanning ≥20s.
	var out []store.Break
	i := 0
	for i < len(cuts) {
		j := i
		for j+1 < len(cuts) && cuts[j+1]-cuts[j] <= 240 {
			j++
		}
		if j > i && cuts[j]-cuts[i] >= 20 {
			out = append(out, store.Break{Start: max64(0, cuts[i]-0.5), End: cuts[j] + 0.5})
		}
		i = j + 1
	}
	return out, nil
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// ---- deletion ----------------------------------------------------------------

// Cut removes breaks from path in place (stream copy, keyframe-snapped).
// Returns the new path (always .mkv) and the summed kept duration.
func Cut(ctx context.Context, set store.Settings, path string, totalDur float64, breaks []store.Break) (string, float64, error) {
	if len(breaks) == 0 {
		return path, totalDur, nil
	}
	sort.Slice(breaks, func(i, j int) bool { return breaks[i].Start < breaks[j].Start })

	// complement of breaks = keep list
	type seg struct{ a, b float64 }
	var keeps []seg
	cursor := 0.0
	for _, br := range breaks {
		if br.Start > cursor+1 {
			keeps = append(keeps, seg{cursor, br.Start})
		}
		if br.End > cursor {
			cursor = br.End
		}
	}
	if totalDur > cursor+1 {
		keeps = append(keeps, seg{cursor, totalDur})
	}
	if len(keeps) == 0 {
		return path, totalDur, fmt.Errorf("breaks cover entire recording; refusing to cut")
	}

	work, err := os.MkdirTemp(filepath.Dir(path), ".helios-cut-*")
	if err != nil {
		return path, totalDur, err
	}
	defer os.RemoveAll(work)

	list := filepath.Join(work, "concat.txt")
	lf, err := os.Create(list)
	if err != nil {
		return path, totalDur, err
	}
	kept := 0.0
	for i, k := range keeps {
		part := filepath.Join(work, fmt.Sprintf("part%03d.ts", i))
		// -ss before -i: fast keyframe seek; -t bounds the copy.
		cmd := exec.CommandContext(ctx, set.FFmpegPath, "-hide_banner", "-loglevel", "error",
			"-ss", fmt.Sprintf("%.3f", k.a), "-i", path,
			"-t", fmt.Sprintf("%.3f", k.b-k.a),
			"-map", "0:v:0", "-map", "0:a?", "-c", "copy",
			"-avoid_negative_ts", "make_zero", "-y", part)
		if err := cmd.Run(); err != nil {
			lf.Close()
			return path, totalDur, fmt.Errorf("extract segment %d: %w", i, err)
		}
		fmt.Fprintf(lf, "file '%s'\n", strings.ReplaceAll(part, "'", `'\''`))
		kept += k.b - k.a
	}
	lf.Close()

	out := strings.TrimSuffix(path, filepath.Ext(path)) + ".cut.mkv"
	cmd := exec.CommandContext(ctx, set.FFmpegPath, "-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "0", "-i", list, "-c", "copy", "-y", out)
	if err := cmd.Run(); err != nil {
		return path, totalDur, fmt.Errorf("concat: %w", err)
	}
	final := strings.TrimSuffix(path, filepath.Ext(path)) + ".mkv"
	_ = os.Remove(path) // drop the original (this is the "deletion" in auto-skip & deletion)
	if err := os.Rename(out, final); err != nil {
		return out, kept, nil // cut succeeded; report the .cut.mkv path
	}
	return final, kept, nil
}
