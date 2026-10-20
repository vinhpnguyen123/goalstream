//go:build integration

package transcode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const fixtures = "../../testdata/matches/test_league/2015-2016"

func fixture(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join(fixtures, rel)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture missing (run `make fixtures`): %v", err)
	}
	return p
}

func TestProbeFixtures(t *testing.T) {
	ctx := context.Background()

	p, err := ProbeFile(ctx, fixture(t, "2015-08-22 - 15-00 Mainz 05 2 - 1 Hannover 96/1_720p.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasVideo || !p.HasAudio || p.Width != 320 || p.Height != 180 {
		t.Fatalf("mainz half: %+v", p)
	}
	if p.DurationMS < 179_000 || p.DurationMS > 181_000 {
		t.Fatalf("mainz half duration %d ms, want ~180000", p.DurationMS)
	}
	if p.FormatBps <= 0 {
		t.Fatalf("MKV must still report a container bitrate: %+v", p)
	}

	p, err = ProbeFile(ctx, fixture(t, "2015-08-29 - 15-00 Team A 0 - 1 Team B/1_720p.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasVideo || p.HasAudio {
		t.Fatalf("no-audio half must probe as video-only: %+v", p)
	}
}

func TestDecodeCheck(t *testing.T) {
	ctx := context.Background()
	if err := DecodeCheck(ctx, fixture(t, "2015-09-05 - 15-00 Team C 1 - 0 Team D/1_720p.mkv")); err != nil {
		t.Fatalf("good file failed the decode check: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "corrupt.mp4")
	os.WriteFile(bad, []byte("definitely not a video file"), 0o600)
	if err := DecodeCheck(ctx, bad); err == nil {
		t.Fatal("garbage passed the decode check")
	}
	if _, err := ProbeFile(ctx, bad); err == nil {
		t.Fatal("garbage probed successfully")
	}
}
