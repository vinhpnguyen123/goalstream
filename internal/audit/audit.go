// Package audit appends to audit_log. Actions and metadata keys are a closed vocabulary
// (IMPLEMENTATION_PLAN §6). The log never contains secrets, hashes or presigned URLs.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
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

// Write appends e. request_id and actor_user_id come from ctx — set by the API's middleware —
// so a row written while serving a request joins to that request's logs and X-Request-ID, and
// to the user who made it. The worker, reaper and ingest carry neither: both stay NULL.
func Write(ctx context.Context, q Execer, e Entry) error {
	meta, err := json.Marshal(e.Metadata)
	if err != nil {
		return fmt.Errorf("audit %s: encoding metadata: %w", e.Action, err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata, request_id, actor_user_id)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::uuid)`,
		e.EntityType, e.EntityID, e.Action, e.ActorType, meta, httpx.RequestID(ctx), userID(ctx))
	if err != nil {
		return fmt.Errorf("audit %s: %w", e.Action, err)
	}
	return nil
}

func userID(ctx context.Context) string {
	id, _ := httpx.UserID(ctx)
	return id
}
