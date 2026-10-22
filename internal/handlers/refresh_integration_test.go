//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
)

// loggedIn registers and logs in one user; it returns the user id and the refresh cookie value.
func (e apiEnv) loggedIn(t *testing.T) (user, refresh string) {
	t.Helper()
	if rec, body := e.call(t, "POST", "/api/v1/auth/register", creds("me@x.com", "correct horse battery"), "203.0.113.9", true, ""); rec.Code != 201 {
		t.Fatalf("register: %d %v", rec.Code, body)
	}
	rec, body := e.call(t, "POST", "/api/v1/auth/login", creds("me@x.com", "correct horse battery"), "203.0.113.9", true, "")
	if rec.Code != 200 {
		t.Fatalf("login: %d %v", rec.Code, body)
	}
	user, _ = e.s.Signer.Verify(data(body)["access_token"].(string))
	return user, refreshCookie(t, rec)
}

// refreshCookie is the gs_refresh value a response set; "" when it cleared the cookie.
func refreshCookie(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.RefreshCookie {
			if c.MaxAge < 0 {
				return ""
			}
			return c.Value
		}
	}
	t.Fatalf("response sets no %s cookie", auth.RefreshCookie)
	return ""
}

// authPost POSTs to an /auth endpoint with the invite cookie and, when given, gs_refresh.
func (e apiEnv) authPost(path, refresh string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.AddCookie(e.cookie)
	if refresh != "" {
		req.AddCookie(&http.Cookie{Name: auth.RefreshCookie, Value: refresh})
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e apiEnv) refresh(t *testing.T, token string) (code int, next, user string) {
	t.Helper()
	rec := e.authPost("/api/v1/auth/refresh", token)
	if rec.Code == 200 {
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		user, _ = e.s.Signer.Verify(data(body)["access_token"].(string))
	}
	return rec.Code, refreshCookie(t, rec), user
}

// ageRotation moves a token's revocation into the past, so a test crosses the grace without
// sleeping 30 seconds.
func (e apiEnv) ageRotation(t *testing.T, token, by string) {
	t.Helper()
	if _, err := e.s.Pool.Exec(context.Background(),
		`UPDATE refresh_tokens SET revoked_at = revoked_at - $2::interval WHERE token_hash = $1`,
		auth.HashRefresh(token), by); err != nil {
		t.Fatal(err)
	}
}

// statuses counts the family's tokens by state: "active", "rotated", "reuse", "logout".
func (e apiEnv) statuses(t *testing.T, token string) map[string]int {
	t.Helper()
	rows, err := e.s.Pool.Query(context.Background(), `
		SELECT COALESCE(revoked_reason, 'active'), count(*) FROM refresh_tokens
		 WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)
		 GROUP BY 1`, auth.HashRefresh(token))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		got[k] = n
	}
	return got
}

// §12 #20: rotation, the grace, and reuse after it.
func TestRefreshRotationGraceAndReuse(t *testing.T) {
	e := jwtAPI(t)
	user, first := e.loggedIn(t)

	code, second, who := e.refresh(t, first)
	if code != 200 || second == "" || second == first || who != user {
		t.Fatalf("rotation: %d, new token %v, user %q", code, second != first, who)
	}
	if got := e.statuses(t, first); got["rotated"] != 1 || got["active"] != 1 || len(got) != 2 {
		t.Fatalf("after rotation: %v, want 1 rotated + 1 active in one family", got)
	}

	// Inside the grace: a second tab replaying `first` gets its own successor, family intact.
	code, third, _ := e.refresh(t, first)
	if code != 200 || third == "" || third == second {
		t.Fatalf("replay inside grace: %d", code)
	}
	if got := e.statuses(t, first); got["active"] != 2 || got["reuse"] != 0 {
		t.Fatalf("after grace replay: %v", got)
	}

	// Past the grace: the replay is treated as theft — the WHOLE family dies, current tokens too.
	e.ageRotation(t, first, "31 seconds")
	if code, cleared, _ := e.refresh(t, first); code != 401 || cleared != "" {
		t.Fatalf("replay after grace: %d, cookie cleared %v", code, cleared == "")
	}
	if got := e.statuses(t, first); got["active"] != 0 || got["reuse"] != 2 {
		t.Fatalf("after reuse: %v, want both successors revoked as reuse", got)
	}
	if code, _, _ := e.refresh(t, second); code != 401 {
		t.Fatalf("a successor still refreshes after reuse: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action='refresh_reuse_detected' AND entity_id=$1
		AND actor_type='system' AND metadata ? 'family_id'`, user); n < 1 {
		t.Fatal("no refresh_reuse_detected audit")
	}
}

// §12 #20: two concurrent refreshes with one token inside the grace both succeed and the family
// survives — the two-tabs / StrictMode case. Run with -race.
func TestRefreshConcurrentSameToken(t *testing.T) {
	e := jwtAPI(t)
	_, token := e.loggedIn(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.authPost("/api/v1/auth/refresh", token).Code
		}()
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("concurrent refreshes: %v, want both 200", codes)
	}
	if got := e.statuses(t, token); got["rotated"] != 1 || got["active"] != 2 || got["reuse"] != 0 {
		t.Fatalf("family after the race: %v, want 1 rotated + 2 active", got)
	}
}

// §12 #20: logout → refresh 401; logout inside a grace mints nothing; logout is idempotent.
func TestLogout(t *testing.T) {
	e := jwtAPI(t)
	_, first := e.loggedIn(t)
	_, second, _ := e.refresh(t, first)

	rec := e.authPost("/api/v1/auth/logout", second)
	if rec.Code != 204 || refreshCookie(t, rec) != "" {
		t.Fatalf("logout: %d", rec.Code)
	}
	if code, _, _ := e.refresh(t, second); code != 401 {
		t.Fatalf("refresh after logout: %d", code)
	}
	// `first` was rotated a moment ago — inside the grace — but its family is logged out.
	if code, _, _ := e.refresh(t, first); code != 401 {
		t.Fatalf("grace replay after logout: %d, want 401", code)
	}
	if got := e.statuses(t, first); got["active"] != 0 {
		t.Fatalf("a token was minted into a logged-out family: %v", got)
	}
	for _, tok := range []string{"", "unknown-token"} {
		if rec := e.authPost("/api/v1/auth/logout", tok); rec.Code != 204 {
			t.Errorf("logout with %q: %d, want 204", tok, rec.Code)
		}
	}
}

func TestRefreshRefusals(t *testing.T) {
	e := jwtAPI(t)
	_, token := e.loggedIn(t)
	for name, tok := range map[string]string{"no cookie": "", "unknown": "not-a-real-token"} {
		if code, cleared, _ := e.refresh(t, tok); code != 401 || cleared != "" {
			t.Errorf("%s: %d", name, code)
		}
	}
	// Expired: 401, and the family is NOT revoked — an expired token proves nothing.
	if _, err := e.s.Pool.Exec(context.Background(),
		`UPDATE refresh_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, auth.HashRefresh(token)); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.refresh(t, token); code != 401 {
		t.Fatalf("expired: %d", code)
	}
	if got := e.statuses(t, token); got["active"] != 1 || got["reuse"] != 0 {
		t.Fatalf("expired token revoked its family: %v", got)
	}

	// Gate first; the refresh rule fails closed.
	req := httptest.NewRequest("POST", "/api/v1/auth/refresh", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 401 || !json.Valid(rec.Body.Bytes()) || errTypeOf(rec) != "gate_required" {
		t.Errorf("no invite cookie: %d %s", rec.Code, rec.Body)
	}
	e.s.Redis = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	down := apiEnv{e.s, e.s.Routes(), e.cookie}
	if rec := down.authPost("/api/v1/auth/refresh", token); rec.Code != 503 {
		t.Errorf("Redis down: %d, want 503", rec.Code)
	}
}

func errTypeOf(rec *httptest.ResponseRecorder) string {
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return errType(body)
}
