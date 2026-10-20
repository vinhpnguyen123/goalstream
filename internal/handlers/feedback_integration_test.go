//go:build integration

package handlers

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func feedbackBody(sess, clip, response string) string {
	return fmt.Sprintf(`{"session_id":%q,"clip_id":%q,"response":%q,"replay_count":0,"watched_full":true}`, sess, clip, response)
}

func (e apiEnv) newSession(t *testing.T) string {
	t.Helper()
	_, body := e.post(t, "/api/v1/sessions")
	return data(body)["id"].(string)
}

func (e apiEnv) count(t *testing.T, query string, args ...any) (n int) {
	t.Helper()
	if err := e.s.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// §12 #14 (first half): the same response twice is one row; the second answer is changed:false.
func TestFeedbackIdempotentByState(t *testing.T) {
	e := setupAPI(t)
	clip, sess := e.seedClip(t, "c", time.Now(), []byte("x")), e.newSession(t)

	rec, body := e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, clip, "loved"))
	if d := data(body); rec.Code != 200 || d["changed"] != true || d["previous"] != nil {
		t.Fatalf("first: %d %v", rec.Code, body)
	}
	rec, body = e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, clip, "loved"))
	if d := data(body); rec.Code != 200 || d["changed"] != false {
		t.Fatalf("second: %d %v", rec.Code, body)
	}
	if n := e.count(t, `SELECT count(*) FROM feedback_events WHERE clip_id=$1`, clip); n != 1 {
		t.Fatalf("%d feedback rows, want 1", n)
	}
}

// §12 #14 (second half): someone else's session is 404 and writes nothing; so is an unknown clip.
func TestFeedbackOwnershipAndMissing(t *testing.T) {
	e := setupAPI(t)
	clip, sess := e.seedClip(t, "c", time.Now(), []byte("x")), e.newSession(t)
	other := e.otherUserSession(t)
	for name, body := range map[string]string{
		"other user's session": feedbackBody(other, clip, "loved"),
		"unknown session":      feedbackBody(uuid.NewString(), clip, "loved"),
		"unknown clip":         feedbackBody(sess, uuid.NewString(), "loved"),
	} {
		if rec, resp := e.postJSON(t, "/api/v1/feedback", body); rec.Code != 404 || errType(resp) != "not_found" {
			t.Errorf("%s: %d %v", name, rec.Code, resp)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM feedback_events`) + e.count(t, `SELECT count(*) FROM seen_clips`); n != 0 {
		t.Fatalf("a rejected request wrote %d rows", n)
	}
}

// §12 #15: presigning does not mark a clip seen; feedback does.
func TestSeenClipsWrittenOnFeedbackNotPlay(t *testing.T) {
	e := setupAPI(t)
	clip, sess := e.seedClip(t, "c", time.Now(), []byte("x")), e.newSession(t)
	if rec, _ := e.get(t, "/api/v1/clips/"+clip+"/play", true); rec.Code != 200 {
		t.Fatalf("play: %d", rec.Code)
	}
	if n := e.count(t, `SELECT count(*) FROM seen_clips`); n != 0 {
		t.Fatalf("/play wrote %d seen_clips rows", n)
	}
	e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, clip, "okay"))
	if n := e.count(t, `SELECT count(*) FROM seen_clips WHERE user_id=$1 AND clip_id=$2`, e.s.DevUserID, clip); n != 1 {
		t.Fatalf("feedback wrote %d seen_clips rows, want 1", n)
	}
}

// Re-rating appends, reports the previous rating, keeps one seen_clips row; summary follows.
func TestFeedbackReRate(t *testing.T) {
	e := setupAPI(t)
	clip, sess := e.seedClip(t, "c", time.Now(), []byte("x")), e.newSession(t)
	var prev any
	for _, resp := range []string{"loved", "not_for_me", "loved"} {
		rec, body := e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, clip, resp))
		if d := data(body); rec.Code != 200 || d["changed"] != true || d["previous"] != prev {
			t.Fatalf("%s: %d %v, want previous %v", resp, rec.Code, body, prev)
		}
		prev = resp
	}
	if n := e.count(t, `SELECT count(*) FROM feedback_events`); n != 3 {
		t.Fatalf("%d events, want 3 (append-only)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM seen_clips`); n != 1 {
		t.Fatalf("%d seen_clips rows, want 1", n)
	}
	_, body := e.get(t, "/api/v1/sessions/"+sess+"/summary", true)
	if d := data(body); d["rated"] != 1.0 || d["loved"] != 1.0 || d["not_for_me"] != 0.0 {
		t.Fatalf("summary %v", body)
	}
}

// A double tap that arrives concurrently: the user lock lets exactly one request per clip
// append. Many clips × many requests released at once, so the race window is actually hit
// (verified: without the lock this fails).
func TestFeedbackConcurrentSameResponse(t *testing.T) {
	e := setupAPI(t)
	sess := e.newSession(t)
	const clips, perClip = 10, 10
	var ids []string
	for i := range clips {
		ids = append(ids, e.seedClip(t, fmt.Sprint(i), time.Now(), []byte("x")))
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	changed := map[string]int{}
	for _, clip := range ids {
		for range perClip {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rec, body := e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, clip, "loved"))
				if rec.Code != 200 {
					t.Errorf("%d %v", rec.Code, body)
				}
				if data(body)["changed"] == true {
					mu.Lock()
					changed[clip]++
					mu.Unlock()
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	for _, clip := range ids {
		if changed[clip] != 1 {
			t.Errorf("clip %s: %d requests reported changed:true, want exactly 1", clip, changed[clip])
		}
	}
	if rows := e.count(t, `SELECT count(*) FROM feedback_events`); rows != clips {
		t.Fatalf("%d rows, want %d (one per clip)", rows, clips)
	}
}

func TestFeedbackValidation(t *testing.T) {
	e := setupAPI(t)
	clip, sess := e.seedClip(t, "c", time.Now(), []byte("x")), e.newSession(t)
	for name, body := range map[string]string{
		"empty":             ``,
		"bad response":      feedbackBody(sess, clip, "meh"),
		"bad session id":    feedbackBody("abc", clip, "loved"),
		"bad clip id":       feedbackBody(sess, "abc", "loved"),
		"negative replay":   fmt.Sprintf(`{"session_id":%q,"clip_id":%q,"response":"okay","replay_count":-1,"watched_full":true}`, sess, clip),
		"no watched_full":   fmt.Sprintf(`{"session_id":%q,"clip_id":%q,"response":"okay","replay_count":0}`, sess, clip),
		"no replay_count":   fmt.Sprintf(`{"session_id":%q,"clip_id":%q,"response":"okay","watched_full":true}`, sess, clip),
		"null watched_full": fmt.Sprintf(`{"session_id":%q,"clip_id":%q,"response":"okay","replay_count":0,"watched_full":null}`, sess, clip),
		"unknown field":     fmt.Sprintf(`{"session_id":%q,"clip_id":%q,"response":"okay","replay_count":0,"watched_full":true,"user_id":"x"}`, sess, clip),
	} {
		if rec, resp := e.postJSON(t, "/api/v1/feedback", body); rec.Code != 400 || errType(resp) != "validation_error" {
			t.Errorf("%s: %d %v", name, rec.Code, resp)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM feedback_events`); n != 0 {
		t.Fatalf("invalid requests wrote %d rows", n)
	}
}

// An ended session still accepts feedback: a retry can land after /end.
func TestFeedbackAfterSessionEnd(t *testing.T) {
	e := setupAPI(t)
	clip, sess := e.seedClip(t, "c", time.Now(), []byte("x")), e.newSession(t)
	e.post(t, "/api/v1/sessions/"+sess+"/end")
	if rec, body := e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, clip, "okay")); rec.Code != 200 || data(body)["changed"] != true {
		t.Fatalf("%d %v", rec.Code, body)
	}
}
