//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (e apiEnv) post(t *testing.T, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return e.postJSON(t, path, "")
}

func (e apiEnv) postJSON(t *testing.T, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:1234"
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("POST %s: not JSON: %q", path, rec.Body)
	}
	return rec, resp
}

func data(body map[string]any) map[string]any {
	d, _ := body["data"].(map[string]any)
	return d
}

// otherUserSession inserts a session owned by a second user.
func (e apiEnv) otherUserSession(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var user, id string
	if err := e.s.Pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name) VALUES ('other@localhost', '!', 'Other')
		RETURNING id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Pool.QueryRow(ctx, `INSERT INTO sessions (user_id) VALUES ($1) RETURNING id`, user).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSessionCreateAndEnd(t *testing.T) {
	e := setupAPI(t)
	rec, body := e.post(t, "/api/v1/sessions")
	if rec.Code != 200 {
		t.Fatalf("create: %d %v", rec.Code, body)
	}
	id, _ := data(body)["id"].(string)
	if uuid.Validate(id) != nil || data(body)["started_at"] == nil || data(body)["ended_at"] != nil {
		t.Fatalf("create body %v", body)
	}
	var owner string
	if err := e.s.Pool.QueryRow(context.Background(), `SELECT user_id FROM sessions WHERE id=$1`, id).Scan(&owner); err != nil || owner != e.s.DevUserID {
		t.Fatalf("owner %q (%v), want the caller %q", owner, err, e.s.DevUserID)
	}

	rec, first := e.post(t, "/api/v1/sessions/"+id+"/end")
	ended, _ := data(first)["ended_at"].(string)
	if rec.Code != 200 || ended == "" {
		t.Fatalf("end: %d %v", rec.Code, first)
	}
	time.Sleep(10 * time.Millisecond)
	rec, second := e.post(t, "/api/v1/sessions/"+id+"/end")
	if rec.Code != 200 || data(second)["ended_at"] != ended {
		t.Fatalf("second end must be a no-op keeping %s: %d %v", ended, rec.Code, second)
	}
}

// Someone else's session, an unknown id and a malformed id all look the same: 404.
func TestSessionScopedTo404(t *testing.T) {
	e := setupAPI(t)
	other := e.otherUserSession(t)
	for _, id := range []string{other, uuid.NewString(), "not-a-uuid"} {
		if rec, body := e.post(t, "/api/v1/sessions/"+id+"/end"); rec.Code != 404 || errType(body) != "not_found" {
			t.Errorf("end %s: %d %v", id, rec.Code, body)
		}
		if rec, body := e.get(t, "/api/v1/sessions/"+id+"/summary", true); rec.Code != 404 || errType(body) != "not_found" {
			t.Errorf("summary %s: %d %v", id, rec.Code, body)
		}
	}
	var ended *time.Time
	if err := e.s.Pool.QueryRow(context.Background(), `SELECT ended_at FROM sessions WHERE id=$1`, other).Scan(&ended); err != nil || ended != nil {
		t.Fatalf("another user's session was ended: %v %v", ended, err)
	}
}

// Each clip counts once, by its latest rating in THIS session.
func TestSessionSummaryCountsLatestPerClip(t *testing.T) {
	e := setupAPI(t)
	ctx := context.Background()
	a, b, c := e.seedClip(t, "a", time.Now(), []byte("x")), e.seedClip(t, "b", time.Now(), []byte("x")), e.seedClip(t, "c", time.Now(), []byte("x"))
	_, body := e.post(t, "/api/v1/sessions")
	sess := data(body)["id"].(string)
	_, body = e.post(t, "/api/v1/sessions")
	otherSess := data(body)["id"].(string)

	base := time.Now().Add(-time.Minute)
	for i, ev := range []struct{ sess, clip, response string }{
		{sess, a, "not_for_me"},
		{sess, a, "loved"}, // re-rated in the session: a is loved, counted once
		{sess, b, "okay"},
		{otherSess, c, "loved"}, // another session: not counted
	} {
		if _, err := e.s.Pool.Exec(ctx, `
			INSERT INTO feedback_events (session_id, user_id, clip_id, response, watched_full, created_at)
			VALUES ($1, $2, $3, $4, true, $5)`, ev.sess, e.s.DevUserID, ev.clip, ev.response, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	rec, body := e.get(t, "/api/v1/sessions/"+sess+"/summary", true)
	d := data(body)
	if rec.Code != 200 || d["rated"] != 2.0 || d["loved"] != 1.0 || d["okay"] != 1.0 || d["not_for_me"] != 0.0 ||
		d["started_at"] == nil || d["ended_at"] != nil {
		t.Fatalf("summary: %d %v", rec.Code, body)
	}
	if rec, body := e.get(t, "/api/v1/sessions/"+otherSess+"/summary", true); data(body)["rated"] != 1.0 {
		t.Fatalf("other session summary: %d %v", rec.Code, body)
	}
}
