package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/storage"
)

// VerifyReport is what `ingest verify-hls` found: clips checked per source_type, and every
// problem, one line each. The catalogue migration (M7) is done when HLS is every clip and
// Problems is empty.
type VerifyReport struct {
	MP4, HLS int
	Problems []Problem
}

type Problem struct{ ClipID, What string }

type verifyClip struct {
	id, prefix, sourceType string
	version                *string
	rungs                  []verifyRung // of the live version, tallest first
}

type verifyRung struct{ height, segments int }

// Verify checks that every clip can still be played the way /play would serve it:
//
//   - every clip: original.mp4 exists (the durability rule — it is the mp4 fallback and the
//     rollback target);
//   - hls clips: the live version has master.m3u8 and an index.m3u8 per rung; every segment a
//     rung's playlist names exists, and it names as many as its renditions row recorded at
//     commit (the folder's contents do not matter — a player requests only what is named);
//   - hls clips: ffprobe opens the master through the local nginx cache, unsigned, the URL /play
//     returns — which makes it open every variant playlist and its first segment, as a player
//     does — and sees one video stream per rung, at the rung's height. hlsRoot is
//     <HLS_BASE_URL>/<bucket>: since M9 MinIO serves hls/ unsigned only to nginx.
//
// Clips run one at a time: ~0.5 s each, a minute for the catalogue. Storage and ffprobe errors
// are problems, not failures of the run; only a database error aborts it.
func Verify(ctx context.Context, pool *pgxpool.Pool, store *storage.Client, hlsRoot string) (VerifyReport, error) {
	clips, err := verifyClips(ctx, pool)
	if err != nil {
		return VerifyReport{}, err
	}
	var r VerifyReport
	for _, c := range clips {
		if c.sourceType == "hls" {
			r.HLS++
		} else {
			r.MP4++
		}
		for _, what := range verifyOne(ctx, store, hlsRoot, c) {
			r.Problems = append(r.Problems, Problem{c.id, what})
		}
	}
	return r, nil
}

// verifyClips reads every clip with the renditions of its LIVE version only (none for mp4).
func verifyClips(ctx context.Context, pool *pgxpool.Pool) ([]verifyClip, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.id::text, c.s3_prefix, c.source_type, c.hls_version::text, r.height, r.segment_count
		  FROM clips c
		  LEFT JOIN renditions r ON c.source_type = 'hls' AND r.clip_id = c.id AND r.hls_version = c.hls_version
		 ORDER BY c.created_at, c.id, r.height DESC`)
	if err != nil {
		return nil, fmt.Errorf("reading clips: %w", err)
	}
	defer rows.Close()
	var clips []verifyClip
	for rows.Next() {
		var c verifyClip
		var height, segments *int
		if err := rows.Scan(&c.id, &c.prefix, &c.sourceType, &c.version, &height, &segments); err != nil {
			return nil, err
		}
		if n := len(clips); n == 0 || clips[n-1].id != c.id {
			clips = append(clips, c)
		}
		if height != nil {
			last := &clips[len(clips)-1]
			last.rungs = append(last.rungs, verifyRung{*height, *segments})
		}
	}
	return clips, rows.Err()
}

func verifyOne(ctx context.Context, store *storage.Client, hlsRoot string, c verifyClip) []string {
	var out []string
	if _, err := store.Size(ctx, c.prefix+"original.mp4"); errors.Is(err, storage.ErrNotFound) {
		out = append(out, "original.mp4 missing")
	} else if err != nil {
		out = append(out, "original.mp4: "+err.Error())
	}
	if c.sourceType != "hls" {
		return out
	}
	if len(c.rungs) == 0 { // the fenced commit writes renditions with the pointer: never expected
		return append(out, "live version "+*c.version+" has no renditions rows")
	}

	base := c.prefix + "hls/" + *c.version + "/"
	keys, err := store.ListKeys(ctx, base)
	if err != nil {
		return append(out, err.Error())
	}
	have := map[string]bool{}
	for _, k := range keys {
		have[strings.TrimPrefix(k, base)] = true
	}
	if !have["master.m3u8"] {
		return append(out, "master.m3u8 missing") // nothing a player could open
	}
	want := make([]int, 0, len(c.rungs))
	for _, r := range c.rungs {
		dir := fmt.Sprintf("%dp", r.height)
		want = append(want, r.height)
		if !have[dir+"/index.m3u8"] {
			out = append(out, dir+"/index.m3u8 missing")
			continue
		}
		// What a player requests is what the playlist names, not what the folder holds: every
		// referenced segment must exist, and the playlist must list what the commit recorded.
		uris, err := segmentURIs(ctx, store, base+dir+"/index.m3u8")
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		var missing []string
		for _, u := range uris {
			if !have[dir+"/"+u] {
				missing = append(missing, u)
			}
		}
		if len(missing) > 0 {
			out = append(out, fmt.Sprintf("%s/index.m3u8: %d of %d referenced segments missing (first: %s)",
				dir, len(missing), len(uris), missing[0]))
		}
		if len(uris) != r.segments {
			out = append(out, fmt.Sprintf("%s/index.m3u8 lists %d segments, renditions says %d", dir, len(uris), r.segments))
		}
	}

	got, err := probeHeights(ctx, hlsRoot+"/"+base+"master.m3u8")
	if err != nil {
		return append(out, err.Error())
	}
	if !slices.Equal(got, want) {
		out = append(out, fmt.Sprintf("ffprobe sees video heights %v, want %v", got, want))
	}
	return out
}

// maxPlaylist bounds a rendition playlist read: a 60 s clip's is under 1 KB.
const maxPlaylist = 1 << 20

// segmentURIs is every segment URI a rendition playlist names, in order: the non-comment lines.
// The worker writes them relative to the playlist (seg_00000.ts …).
func segmentURIs(ctx context.Context, store *storage.Client, key string) ([]string, error) {
	body, err := store.Open(ctx, key, "")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxPlaylist))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}
	var uris []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			uris = append(uris, line)
		}
	}
	return uris, nil
}

// signature matches a URL's query string. The nginx URL carries none today, but a query string
// may be a credential (a presign signature) and ffprobe echoes the URL in its errors, so one
// never leaves this function unredacted.
var signature = regexp.MustCompile(`\?[^\s'"]*`)

// probeHeights is the heights of every video stream ffprobe finds behind the master, tallest
// first. Variants are fetched unsigned by relative URI, exactly as the browser does.
func probeHeights(ctx context.Context, url string) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, _, err := run(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_entries", "stream=codec_type,height", url)
	if err != nil {
		return nil, fmt.Errorf("ffprobe master: %s", strings.TrimSpace(signature.ReplaceAllString(err.Error(), "?<redacted>")))
	}
	var o struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &o); err != nil {
		return nil, fmt.Errorf("ffprobe master: parsing json: %w", err)
	}
	var hs []int
	for _, s := range o.Streams {
		if s.CodecType == "video" {
			hs = append(hs, s.Height)
		}
	}
	slices.SortFunc(hs, func(a, b int) int { return b - a })
	return hs, nil
}
