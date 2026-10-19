// Package middleware is the HTTP chain, in the order of docs/API.md § Middleware order.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// BodyLimit is the API-wide request body ceiling (docs/API.md).
const BodyLimit = 64 << 10

// RequestID mints an ID for every request. A client-sent X-Request-ID is ignored: a caller
// must not be able to make two requests share an ID in the logs.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		_, _ = rand.Read(b) // crypto/rand.Read never fails (Go ≥ 1.24)
		id := hex.EncodeToString(b)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), id)))
	})
}

// Recover turns a panic into 500 internal, logged with the request ID and stack.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v) // the server's own "abort this response" signal
				}
				httpx.Log(r.Context()).Error("panic", "value", fmt.Sprint(v), "stack", string(debug.Stack()))
				httpx.Error(w, r, http.StatusInternalServerError, "internal", "something went wrong")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// LimitBody caps the body; a handler's decode then fails with *http.MaxBytesError → 413.
func LimitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, BodyLimit)
		next.ServeHTTP(w, r)
	})
}
