package ingest

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
