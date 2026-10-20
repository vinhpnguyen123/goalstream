//go:build integration

package ingest

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/storage/storagetest"
	"github.com/vinhpnguyen123/goalstream/internal/vocab"
)

const testdata = "../../testdata"

type env struct {
	pool  *pgxpool.Pool
	store *storage.Client
	r     *Runner
}

func setup(t *testing.T) env {
	t.Helper()
	if _, err := os.Stat(filepath.Join(testdata, "matches")); err != nil {
		t.Fatalf("fixtures missing (run `make fixtures`): %v", err)
	}
	ctx := context.Background()
	cfg := storagetest.Config(t) // empties the test bucket
	url := os.Getenv("TEST_DATABASE_URL")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE clips, matches, audit_log CASCADE; DELETE FROM vocab WHERE kind <> 'tag'`); err != nil {
		t.Fatal(err)
	}
	store := storage.New(cfg.S3)
	return env{pool, store, &Runner{
		Pool: pool, Store: store, Cfg: cfg.Ingest,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
}

func (e env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// §12 #1, #2, #4, #5, #6 in one run over the fixtures (cuts are the slow part).
func TestIngestFixtures(t *testing.T) {
	ctx := context.Background()
	e := setup(t)

	sum, err := e.r.Run(ctx, testdata)
	if err != nil {
		t.Fatal(err)
	}

	// 4 match folders (the .cache dir is skipped); X 0 - 0 Y is rejected before any video work.
	if sum.Matches != 4 {
		t.Errorf("matches scanned %d, want 4", sum.Matches)
	}
	if sum.Ingested != 3 || sum.Present != 0 {
		t.Errorf("ingested %d present %d, want 3/0", sum.Ingested, sum.Present)
	}
	wantRejected := map[string]int{
		ReasonTooShort:           1, // Mainz h1@170000 → 25 s
		ReasonNoAudioStream:      1, // Team A
		ReasonNotShown:           1, // Mainz h1@100000
		ReasonMissingVideo:       1, // Team C h2, no 2_720p.mkv
		ReasonBadAnnotation:      1, // Team C "3 - 00:10"
		ReasonPositionOutOfRange: 1, // Team C 999999 ms > 180 s
		ReasonBadMatchMetadata:   1, // X 0 - 0 Y, no gameHomeTeam
	}
	for reason, n := range wantRejected {
		if sum.Rejected[reason] != n {
			t.Errorf("rejected %s = %d, want %d (all: %v)", reason, sum.Rejected[reason], n, sum.Rejected)
		}
	}
	if len(sum.Rejected) != len(wantRejected) {
		t.Errorf("unexpected rejection reasons: %v", sum.Rejected)
	}
	// Mainz: 3 good + 1 too short; Team A: 1 → 5 cuts. Nothing is cut for rejections decided
	// from the annotation or the filesystem alone.
	if sum.Cuts != 5 {
		t.Errorf("cuts %d, want 5", sum.Cuts)
	}

	// #1/#4: the three clips, including the same minute in BOTH halves (dedup-key regression).
	rows, err := e.pool.Query(ctx, `
		SELECT half, position_ms, title, scoring_team, is_home, match_minute, width, height,
		       source_bitrate, duration_ms, vector_norm(embedding), s3_prefix, id::text
		  FROM clips ORDER BY half, position_ms`)
	if err != nil {
		t.Fatal(err)
	}
	type clip struct {
		half, pos              int
		title, team            string
		home                   bool
		minute, w, h, bps, dur int
		norm                   float64
		prefix, id             string
	}
	var clips []clip
	for rows.Next() {
		var c clip
		if err := rows.Scan(&c.half, &c.pos, &c.title, &c.team, &c.home, &c.minute, &c.w, &c.h,
			&c.bps, &c.dur, &c.norm, &c.prefix, &c.id); err != nil {
			t.Fatal(err)
		}
		clips = append(clips, c)
	}
	want := []struct {
		half, pos int
		title     string
		team      string
		home      bool
		minDur    int
	}{
		{1, 5000, "Mainz 05 vs Hannover 96 — Goal 1'", "Mainz 05", true, 49_000},       // pre-roll clamped: 50 s
		{1, 60000, "Mainz 05 vs Hannover 96 — Goal 2'", "Mainz 05", true, 59_000},      // 60 s
		{2, 60000, "Mainz 05 vs Hannover 96 — Goal 47'", "Hannover 96", false, 59_000}, // same minute, other half
	}
	if len(clips) != len(want) {
		t.Fatalf("got %d clips, want %d", len(clips), len(want))
	}
	for i, w := range want {
		c := clips[i]
		if c.half != w.half || c.pos != w.pos || c.title != w.title || c.team != w.team || c.home != w.home {
			t.Errorf("clip %d: %+v, want %+v", i, c, w)
		}
		if c.w != 320 || c.h != 180 || c.bps <= 0 || c.dur < w.minDur || c.dur > 61_000 {
			t.Errorf("clip %d probe data: %dx%d %d bps %d ms", i, c.w, c.h, c.bps, c.dur)
		}
		if math.Abs(c.norm-1) > 1e-5 {
			t.Errorf("clip %d embedding norm %f, want 1", i, c.norm)
		}
		if c.prefix != "clips/"+c.id+"/" {
			t.Errorf("clip %d s3_prefix %q is not a prefix of its own id", i, c.prefix)
		}
	}

	// Bucket = rows: exactly one original.mp4 per clip, nothing for rejected events.
	keys, err := e.store.ListKeys(ctx, "clips/")
	if err != nil {
		t.Fatal(err)
	}
	var wantKeys []string
	for _, c := range clips {
		wantKeys = append(wantKeys, c.prefix+"original.mp4")
	}
	sort.Strings(keys)
	sort.Strings(wantKeys)
	if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("bucket keys %v, want %v", keys, wantKeys)
	}

	// Vocab: only scoring teams of INGESTED clips (Team A's goal was rejected before vocab).
	if n := e.count(t, `SELECT count(*) FROM vocab WHERE kind='team'`); n != 2 {
		t.Errorf("team slots %d, want 2", n)
	}
	if n := e.count(t, `SELECT count(*) FROM vocab WHERE kind='competition' AND value='test_league'`); n != 1 {
		t.Error("competition must come from the league folder name")
	}

	// Audit: rejections are recorded against the match, never a non-existent clip.
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action='clip_ingested' AND entity_type='clip'`); n != 3 {
		t.Errorf("clip_ingested audits %d, want 3", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action='clip_rejected' AND entity_type='match'`); n != 6 {
		t.Errorf("clip_rejected audits %d, want 6", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action='match_rejected' AND metadata->>'reason'='bad_match_metadata'`); n != 1 {
		t.Errorf("match_rejected audits %d, want 1", n)
	}

	// tags_todo.csv: header + one line per untagged clip.
	path := filepath.Join(t.TempDir(), "tags_todo.csv")
	n, err := WriteTagsTodo(ctx, e.pool, path)
	if err != nil || n != 3 {
		t.Fatalf("WriteTagsTodo = %d, %v", n, err)
	}
	f, _ := os.Open(path)
	recs, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil || len(recs) != 4 || strings.Join(recs[0], ",") != "clip_id,title,match_minute,s3_key,tags" {
		t.Fatalf("tags_todo.csv: %v %v", recs, err)
	}

	// #3 idempotency: a second run ingests nothing, uploads nothing, and never re-cuts an
	// ingested event. Previously REJECTED events are cut and judged again on purpose
	// (QC thresholds are configurable) — here the too_short and the no-audio goal: 2 cuts.
	sum2, err := e.r.Run(ctx, testdata)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.Ingested != 0 || sum2.Present != 3 || sum2.Cuts != 2 {
		t.Errorf("re-run: ingested %d present %d cuts %d, want 0/3/2", sum2.Ingested, sum2.Present, sum2.Cuts)
	}
	keys2, _ := e.store.ListKeys(ctx, "clips/")
	if len(keys2) != len(keys) || e.count(t, `SELECT count(*) FROM clips`) != 3 {
		t.Errorf("re-run changed storage or rows: %d keys, %d rows", len(keys2), e.count(t, `SELECT count(*) FROM clips`))
	}
}

// §12 #7: a full team vocabulary aborts the run loudly, and nothing is uploaded for the
// clip that could not be embedded.
func TestIngestAbortsWhenVocabIsFull(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	for i := 0; i < 64; i++ {
		if _, err := e.pool.Exec(ctx, `INSERT INTO vocab (kind, value, slot) VALUES ('team', $1, $2)`,
			fmt.Sprintf("prefilled-%02d", i), i); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.r.Run(ctx, testdata)
	if !errors.Is(err, vocab.ErrFull) || !strings.Contains(err.Error(), `"team"`) {
		t.Fatalf("want vocab.ErrFull naming the team kind, got %v", err)
	}
	if keys, _ := e.store.ListKeys(ctx, "clips/"); len(keys) != 0 {
		t.Fatalf("objects uploaded despite the abort: %v", keys)
	}
	if n := e.count(t, `SELECT count(*) FROM clips`); n != 0 {
		t.Fatalf("%d clips inserted despite the abort", n)
	}
}
