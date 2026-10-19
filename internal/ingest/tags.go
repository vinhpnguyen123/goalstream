package ingest

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/audit"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
	"github.com/vinhpnguyen123/goalstream/internal/vocab"
)

// LoadTags applies a hand-filled tags sheet (tags separated by "|") in ONE transaction, then
// rebuilds every embedding in the same transaction. An unknown tag, unknown clip or malformed
// line aborts with its line number and applies nothing. Rows left blank are not yet labelled
// and are skipped; rows whose tags already match are no-ops, so re-loading a sheet is safe.
// Returns how many clips changed.
func LoadTags(ctx context.Context, pool *pgxpool.Pool, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(records) == 0 {
		return 0, fmt.Errorf("%s is empty", path)
	}
	col := map[string]int{}
	for i, h := range records[0] {
		col[strings.TrimSpace(h)] = i
	}
	idCol, ok1 := col["clip_id"]
	tagCol, ok2 := col["tags"]
	if !ok1 || !ok2 {
		return 0, fmt.Errorf("%s: header must contain clip_id and tags", path)
	}

	changed := 0
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		known, err := vocab.TagSlots(ctx, tx)
		if err != nil {
			return err
		}
		for i, rec := range records[1:] {
			line := i + 2 // 1-based, after the header
			tags, err := parseTags(rec[tagCol], known)
			if err != nil {
				return fmt.Errorf("line %d: %w", line, err)
			}
			if len(tags) == 0 {
				continue
			}
			id := strings.TrimSpace(rec[idCol])
			if uuid.Validate(id) != nil {
				return fmt.Errorf("line %d: clip_id %q is not a UUID", line, id)
			}
			var current []string
			err = tx.QueryRow(ctx, `SELECT tags FROM clips WHERE id = $1 FOR UPDATE`, id).Scan(&current)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("line %d: unknown clip_id %s", line, id)
			} else if err != nil {
				return fmt.Errorf("line %d: %w", line, err)
			}
			if slices.Equal(current, tags) {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE clips SET tags = $2 WHERE id = $1`, id, tags); err != nil {
				return fmt.Errorf("line %d: updating tags: %w", line, err)
			}
			if err := audit.Write(ctx, tx, audit.Entry{
				EntityType: "clip", EntityID: id, Action: "tags_updated", ActorType: "system",
				Metadata: map[string]any{"tags": tags},
			}); err != nil {
				return err
			}
			changed++
		}
		_, err = recommend.ReembedAll(ctx, tx)
		return err
	})
	return changed, err
}

// parseTags splits "a | b|a" into sorted, de-duplicated tags, each of which must be in vocab.
func parseTags(cell string, known map[string]int) ([]string, error) {
	var out []string
	for _, t := range strings.Split(cell, "|") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if _, ok := known[t]; !ok {
			return nil, fmt.Errorf("unknown tag %q", t)
		}
		out = append(out, t)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// WriteTagsTodo regenerates the hand-labelling sheet from the database — every clip whose
// tags are still empty — so re-runs never duplicate lines. Written to a temp file and renamed,
// so an interrupted run never leaves a half-written sheet.
func WriteTagsTodo(ctx context.Context, pool *pgxpool.Pool, path string) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, title, match_minute, s3_prefix || 'original.mp4'
		  FROM clips WHERE tags = '{}' ORDER BY created_at, id`)
	if err != nil {
		return 0, fmt.Errorf("listing untagged clips: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tags_todo-*.csv")
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	w := csv.NewWriter(tmp)
	w.Write([]string{"clip_id", "title", "match_minute", "s3_key", "tags"})
	var id, title, key string
	var minute, n int
	_, err = pgx.ForEachRow(rows, []any{&id, &title, &minute, &key}, func() error {
		n++
		return w.Write([]string{id, title, strconv.Itoa(minute), key, ""})
	})
	if err != nil {
		tmp.Close()
		return 0, fmt.Errorf("writing tags_todo: %w", err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		tmp.Close()
		return 0, fmt.Errorf("writing tags_todo: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), path)
}
