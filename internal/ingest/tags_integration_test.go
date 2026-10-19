//go:build integration

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vinhpnguyen123/goalstream/internal/recommend"
	"github.com/vinhpnguyen123/goalstream/internal/vocab"
)

// seedClips inserts n clips straight into the DB (no ffmpeg: tags never touch media).
func seedClips(t *testing.T, e env, n int) []string {
	t.Helper()
	ctx := context.Background()
	var ids []string
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		var matchID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO matches (soccernet_path, home_team, away_team, competition, season)
			VALUES ('l/s/m', 'Home FC', 'Away FC', 'l', 's') RETURNING id`).Scan(&matchID); err != nil {
			return err
		}
		team, _ := vocab.Slot(ctx, tx, "team", "Home FC")
		comp, _ := vocab.Slot(ctx, tx, "competition", "l")
		season, err := vocab.Slot(ctx, tx, "season", "s")
		if err != nil {
			return err
		}
		vec, _ := recommend.Embed(recommend.Features{TeamSlot: &team, CompSlot: comp, SeasonSlot: season, MatchMinute: 10})
		for i := range n {
			id := uuid.NewString()
			if _, err := tx.Exec(ctx, `
				INSERT INTO clips (id, match_id, half, position_ms, event_type, title, scoring_team, is_home,
				                   match_minute, s3_prefix, width, height, source_bitrate, duration_ms, embedding)
				VALUES ($1,$2,1,$3,'Goal','t','Home FC',true,10,'clips/x/',320,180,1,60000,$4::vector)`,
				id, matchID, i*1000, recommend.Literal(vec)); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func writeSheet(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tags.csv")
	body := "clip_id,title,match_minute,s3_key,tags\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadTagsAppliesAndReembeds(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	ids := seedClips(t, e, 2)
	sheet := writeSheet(t,
		ids[0]+",t,10,k, late_goal | header|header",
		ids[1]+",t,10,k,", // not labelled yet → untouched
	)

	n, err := LoadTags(ctx, e.pool, sheet)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed %d clips, want 1", n)
	}
	var tags []string
	if err := e.pool.QueryRow(ctx, `SELECT tags FROM clips WHERE id=$1`, ids[0]).Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tags, ",") != "header,late_goal" {
		t.Fatalf("tags %v, want sorted and de-duplicated", tags)
	}
	// header = tag slot 0, late_goal = 10 (seeded); tag block starts at dim 80, arrays are 1-based.
	if got := e.count(t, `SELECT count(*) FROM clips WHERE id=$1
		AND (embedding::real[])[81] > 0 AND (embedding::real[])[91] > 0
		AND abs(vector_norm(embedding) - 1) < 1e-5`, ids[0]); got != 1 {
		t.Fatal("tag dims not set in the rebuilt, L2-normalized embedding")
	}
	if got := e.count(t, `SELECT count(*) FROM clips WHERE id=$1 AND (embedding::real[])[81] = 0`, ids[1]); got != 1 {
		t.Fatal("untagged clip gained a tag dim")
	}
	if got := e.count(t, `SELECT count(*) FROM audit_log WHERE action='tags_updated' AND entity_id=$1`, ids[0]); got != 1 {
		t.Fatalf("tags_updated audits: %d, want 1", got)
	}

	// Re-loading the same sheet is a no-op: nothing changed, nothing audited.
	if n, err := LoadTags(ctx, e.pool, sheet); err != nil || n != 0 {
		t.Fatalf("reload: n=%d err=%v, want 0, nil", n, err)
	}
	if got := e.count(t, `SELECT count(*) FROM audit_log WHERE action='tags_updated'`); got != 1 {
		t.Fatalf("reload audited again: %d rows", got)
	}
}

func TestLoadTagsAbortsWholeSheet(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, line2, wantErr string
	}{
		{"unknown tag", "{id1},t,10,k,bicycle_kick", `line 3: unknown tag "bicycle_kick"`},
		{"unknown clip", uuid.NewString() + ",t,10,k,header", "line 3: unknown clip_id"},
		{"not a uuid", "nope,t,10,k,header", `line 3: clip_id "nope" is not a UUID`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			ids := seedClips(t, e, 2)
			// Line 2 is valid; line 3 is bad → line 2 must NOT be applied either.
			sheet := writeSheet(t, ids[0]+",t,10,k,header", strings.ReplaceAll(tc.line2, "{id1}", ids[1]))
			_, err := LoadTags(ctx, e.pool, sheet)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err %v, want %q", err, tc.wantErr)
			}
			if got := e.count(t, `SELECT count(*) FROM clips WHERE tags <> '{}'`); got != 0 {
				t.Fatalf("%d clip(s) tagged despite the abort", got)
			}
			if got := e.count(t, `SELECT count(*) FROM audit_log`); got != 0 {
				t.Fatalf("%d audit row(s) despite the abort", got)
			}
		})
	}
}

func TestReembedAllFailsOnMissingVocab(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	seedClips(t, e, 1)
	if _, err := e.pool.Exec(ctx, `DELETE FROM vocab WHERE kind='team'`); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		_, err := recommend.ReembedAll(ctx, tx)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "missing from vocab") {
		t.Fatalf("err %v, want a missing-vocab abort", err)
	}
}
