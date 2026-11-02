//go:build integration

package transcode

import (
	"bufio"
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func clipFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("../../testdata", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture missing (run `make fixtures`): %v", err)
	}
	return p
}

// Every rung of the ladder derived from the MEASURED bitrate is produced, and segment N starts
// at the same instant in every rung — the property that makes mid-stream switching clean.
func TestEncodeLadderAligned(t *testing.T) {
	ctx := context.Background()
	src := clipFixture(t, "clip_720p_low.mp4")
	p, err := ProbeFile(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	rungs, err := Ladder(p.VideoBps, p.Height)
	if err != nil {
		t.Fatal(err)
	}
	if len(rungs) < 2 {
		t.Fatalf("fixture measured %d bps → %d rung(s); alignment needs ≥ 2 (regenerate fixtures)", p.VideoBps, len(rungs))
	}
	out := t.TempDir()
	var rds []Rendition
	for _, r := range rungs {
		rd, err := EncodeRendition(ctx, src, out, r)
		if err != nil {
			t.Fatal(err)
		}
		rds = append(rds, rd)
	}

	for _, rd := range rds {
		exact := float64(p.Width) * float64(rd.Height) / float64(p.Height)
		if rd.Width%2 != 0 || math.Abs(float64(rd.Width)-exact) > 2 {
			t.Errorf("%s width %d, want even and ≈ %.1f", rd.Dir(), rd.Width, exact)
		}
		if rd.Segments != rds[0].Segments {
			t.Errorf("%s has %d segments, %s has %d", rd.Dir(), rd.Segments, rds[0].Dir(), rds[0].Segments)
		}
		durs := extinf(t, filepath.Join(out, rd.Dir(), "index.m3u8"))
		for i, d := range durs[:len(durs)-1] { // the last segment may be shorter
			if math.Abs(d-4) > 0.05 {
				t.Errorf("%s segment %d lasts %.3f s, want 4.000", rd.Dir(), i, d)
			}
		}
	}
	for n := 0; n < rds[0].Segments; n++ {
		first := firstPTS(t, ctx, filepath.Join(out, rds[0].Dir(), segName(n)))
		for _, rd := range rds[1:] {
			if got := firstPTS(t, ctx, filepath.Join(out, rd.Dir(), segName(n))); math.Abs(got-first) > 0.04 { // 1 frame at 25 fps
				t.Errorf("segment %d starts at %.3f in %s but %.3f in %s", n, got, rd.Dir(), first, rds[0].Dir())
			}
		}
	}

	thumb := filepath.Join(out, "thumb.jpg")
	if err := Thumbnail(ctx, src, thumb, p.DurationMS); err != nil {
		t.Fatal(err)
	}
	if tp, err := ProbeFile(ctx, thumb); err != nil || tp.Height != 360 {
		t.Fatalf("thumbnail probe = %+v, %v; want a 360p image", tp, err)
	}
}

// A 576p source has no 720p rung: the ladder starts at 480p (§10 example D) and nothing is upscaled.
func TestEncodeNoUpscale(t *testing.T) {
	ctx := context.Background()
	src := clipFixture(t, "clip_576p.mp4")
	p, err := ProbeFile(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	rungs, err := Ladder(p.VideoBps, p.Height)
	if err != nil {
		t.Fatal(err)
	}
	if rungs[0].Height != 480 {
		t.Fatalf("top rung %dp, want 480p", rungs[0].Height)
	}
	rd, err := EncodeRendition(ctx, src, t.TempDir(), rungs[0])
	if err != nil {
		t.Fatal(err)
	}
	if rd.Width > p.Width {
		t.Fatalf("480p rendition is %d wide, source %d", rd.Width, p.Width)
	}
}

func segName(n int) string { return "seg_" + leftPad(n) + ".ts" }

func leftPad(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 5-len(s)) + s
}

// extinf returns the segment durations listed in a media playlist.
func extinf(t *testing.T, playlist string) []float64 {
	t.Helper()
	f, err := os.Open(playlist)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var durs []float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "#EXTINF:"); ok {
			d, err := strconv.ParseFloat(strings.TrimSuffix(v, ","), 64)
			if err != nil {
				t.Fatalf("%s: %v", playlist, err)
			}
			durs = append(durs, d)
		}
	}
	return durs
}

// firstPTS is the presentation time of a segment's first video packet.
func firstPTS(t *testing.T, ctx context.Context, seg string) float64 {
	t.Helper()
	out, _, err := run(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts_time", "-read_intervals", "%+#1", "-of", "csv=p=0", seg)
	if err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseFloat(strings.Trim(strings.Split(string(out), "\n")[0], " ,"), 64)
	if err != nil {
		t.Fatalf("%s: pts %q: %v", seg, out, err)
	}
	return v
}
