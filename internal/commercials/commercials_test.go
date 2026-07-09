package commercials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEDL(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.edl")
	content := "0.00\t92.50\t0\n" + // leading break, action 0 (cut)
		"651.20\t831.90\t3\n" + // action 3 (commercial)
		"900\t890\t0\n" + // end <= start: dropped
		"garbage line\n" +
		"1000.0\t1010.0\t1\n" // action 1 (mute): not an ad
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseEDL(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 breaks, got %d: %+v", len(got), got)
	}
	if got[0].Start != 0 || got[0].End != 92.5 || got[1].Start != 651.2 || got[1].End != 831.9 {
		t.Fatalf("unexpected breaks: %+v", got)
	}
}
