// Package httpx is the response envelope and request ID shared by middleware and handlers
// (docs/API.md § Envelope). It exists so the two packages do not import each other.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

type ctxKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Log returns the default logger tagged with the request ID, so every line can be joined to
// the response a user reports.
func Log(ctx context.Context) *slog.Logger {
	return slog.Default().With("request_id", RequestID(ctx))
}

// JSON writes {"data": …, "request_id": …}.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	write(w, status, map[string]any{"data": data, "request_id": RequestID(r.Context())})
}

// Error writes {"error": {type, message}, "request_id": …}. typ must be one of the closed set
// in docs/API.md; msg is shown to users, so it never carries internals.
func Error(w http.ResponseWriter, r *http.Request, status int, typ, msg string) {
	ErrorWith(w, r, status, typ, msg, nil)
}

// ErrorWith is Error plus fields inside "error" — job_active carries the active job_id.
func ErrorWith(w http.ResponseWriter, r *http.Request, status int, typ, msg string, extra map[string]string) {
	e := map[string]string{"type": typ, "message": msg}
	for k, v := range extra {
		e[k] = v
	}
	write(w, status, map[string]any{"error": e, "request_id": RequestID(r.Context())})
}

// Internal logs err with the request ID and answers 500 without details.
func Internal(w http.ResponseWriter, r *http.Request, err error) {
	Log(r.Context()).Error("internal error", "method", r.Method, "path", r.URL.Path, "err", err)
	Error(w, r, http.StatusInternalServerError, "internal", "something went wrong")
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the status is already sent; nothing left to report to
}

type userKey struct{}

// WithUserID is set only by the auth middleware. Handlers read the caller from here, never
// from the URL or the body.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userKey{}, id)
}

func UserID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userKey{}).(string)
	return id, ok && id != ""
}
