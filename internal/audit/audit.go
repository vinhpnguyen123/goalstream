// Package audit appends to audit_log. Actions and metadata keys are a closed vocabulary
// (IMPLEMENTATION_PLAN §6). The log never contains secrets, hashes or presigned URLs.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

type Entry struct {
	EntityType string // clip | match | job | user
	EntityID   string // UUID
	Action     string
	ActorType  string // user | admin | worker | system
	Metadata   map[string]any
}

// Execer is satisfied by *pgxpool.Pool and pgx.Tx, so an entry can share a transaction with
// the state change it records.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func Write(ctx context.Context, q Execer, e Entry) error {
	meta, err := json.Marshal(e.Metadata)
	if err != nil {
		return fmt.Errorf("audit %s: encoding metadata: %w", e.Action, err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata)
		VALUES ($1, $2, $3, $4, $5)`, e.EntityType, e.EntityID, e.Action, e.ActorType, meta)
	if err != nil {
		return fmt.Errorf("audit %s: %w", e.Action, err)
	}
	return nil
}
