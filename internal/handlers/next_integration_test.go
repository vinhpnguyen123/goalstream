//go:build integration

package handlers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (e apiEnv) next(t *testing.T, sess string, exclude ...string) map[string]any {
	t.Helper()
	path := "/api/v1/sessions/" + sess + "/next"
	if len(exclude) > 0 {
		path += "?exclude=" + strings.Join(exclude, ",")
	}
	rec, body := e.get(t, path, true)
	if rec.Code != 200 {
		t.Fatalf("next: %d %v", rec.Code, body)
	}
	return data(body)
}

// Never a seen (rated) or excluded clip; every remaining clip is reachable.
func TestNextSkipsSeenAndExcluded(t *testing.T) {
	e := setupAPI(t)
	sess := e.newSession(t)
	var ids []string
	for i := range 6 {
		ids = append(ids, e.seedClip(t, fmt.Sprint(i), time.Now(), []byte("x")))
	}
	for _, rated := range ids[:2] {
		e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, rated, "okay"))
	}
	excluded, eligible := ids[2:4], ids[4:]

	served := map[string]int{}
	for range 100 {
		d := e.next(t, sess, excluded...)
		clip, _ := d["clip"].(map[string]any)
		if clip == nil || d["mode"] != "cold" || d["reason"] != "getting to know you" || d["exhausted"] != false {
			t.Fatalf("next: %v", d)
		}
		served[clip["id"].(string)]++
	}
	for id := range served {
		if id != eligible[0] && id != eligible[1] {
			t.Fatalf("served %s, which is seen or excluded", id)
		}
	}
	if len(served) != 2 {
		t.Fatalf("100 picks reached %d of 2 eligible clips: %v", len(served), served)
	}
}

func TestNextExhausted(t *testing.T) {
	e := setupAPI(t)
	sess := e.newSession(t)
	if d := e.next(t, sess); d["clip"] != nil || d["exhausted"] != true {
		t.Fatalf("empty catalogue: %v", d)
	}
	a, b := e.seedClip(t, "a", time.Now(), []byte("x")), e.seedClip(t, "b", time.Now(), []byte("x"))
	e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, a, "loved"))
	if d := e.next(t, sess, b); d["clip"] != nil || d["exhausted"] != true || d["mode"] != "cold" {
		t.Fatalf("all seen or excluded: %v", d)
	}
}

func TestNextScopedAndValidated(t *testing.T) {
	e := setupAPI(t)
	sess := e.newSession(t)
	e.seedClip(t, "c", time.Now(), []byte("x"))
	for _, id := range []string{e.otherUserSession(t), uuid.NewString(), "not-a-uuid"} {
		if rec, body := e.get(t, "/api/v1/sessions/"+id+"/next", true); rec.Code != 404 || errType(body) != "not_found" {
			t.Errorf("session %s: %d %v", id, rec.Code, body)
		}
	}

	many := func(n int) string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = uuid.NewString()
		}
		return strings.Join(ids, ",")
	}
	for _, tc := range []struct {
		exclude string
		code    int
	}{
		{many(101), 400},
		{"abc", 400},
		{uuid.NewString() + ",", 400},
		{many(100), 200},
	} {
		rec, body := e.get(t, "/api/v1/sessions/"+sess+"/next?exclude="+tc.exclude, true)
		if rec.Code != tc.code || (tc.code == 400 && errType(body) != "validation_error") {
			t.Errorf("exclude %.40q…: %d %v", tc.exclude, rec.Code, body)
		}
	}
}
