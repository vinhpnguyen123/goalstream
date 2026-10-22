//go:build integration

package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/db"
)

// §12 #21 for the middleware itself. Admin routes arrive in M5; each one then joins the
// "non-admin → 404" table in its handler tests.
func TestRequireAdmin(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users CASCADE`); err != nil {
		t.Fatal(err)
	}
	var user string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('me@x.com', '!') RETURNING id`).Scan(&user); err != nil {
		t.Fatal(err)
	}

	signer := auth.NewSigner(strings.Repeat("k", 32), 15*time.Minute)
	h := Auth("jwt", "", signer)(RequireAdmin(pool)(ok))
	call := func(id string) (int, map[string]any) {
		tok, _, _ := signer.Issue(id, time.Now())
		req := httptest.NewRequest("POST", "/api/v1/admin/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	notFound := func(name string, code int, body map[string]any) {
		t.Helper()
		e, _ := body["error"].(map[string]any)
		if code != http.StatusNotFound || e["type"] != "not_found" || e["message"] != "not found" {
			t.Errorf("%s: %d %v, want the router's plain 404", name, code, body)
		}
	}

	code, body := call(user)
	notFound("not an admin", code, body)

	// The same access token, before and after each change: is_admin is read per request, never
	// carried in the token, so a promotion or demotion needs no new login.
	if _, err := auth.SetAdmin(ctx, pool, "me@x.com", true); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(user); code != 204 {
		t.Fatalf("after promotion: %d, want 204", code)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET is_admin = false WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	code, body = call(user)
	notFound("demoted, next request", code, body)

	// A valid token whose user no longer exists is still a 404, not a 500.
	code, body = call(uuid.NewString())
	notFound("deleted user", code, body)
}
