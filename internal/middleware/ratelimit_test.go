package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

var ok = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })

// A Redis nobody listens on: connection refused, immediately.
func deadRedis() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
}

func TestStoreDownFailsClosedOrOpen(t *testing.T) {
	for _, tc := range []struct {
		rule Rule
		want int
	}{
		{RuleGate, 503},    // gate/auth: no limiter → no access
		{RuleDefault, 204}, // everything else keeps serving
	} {
		h := RateLimit(deadRedis(), tc.rule, Global)(ok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != tc.want {
			t.Errorf("rule %s with Redis down: %d, want %d", tc.rule.Name, rec.Code, tc.want)
		}
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, remote, header, value, want string
	}{
		{"remote addr", "203.0.113.7:5555", "", "", "203.0.113.7"},
		{"header ignored when not configured", "203.0.113.7:5555", "", "1.2.3.4", "203.0.113.7"},
		{"configured header wins", "10.0.0.1:5555", "X-Real-IP", "198.51.100.2", "198.51.100.2"},
		{"first of a list", "10.0.0.1:5555", "X-Forwarded-For", " 198.51.100.2 , 10.0.0.9", "198.51.100.2"},
		{"configured but absent", "10.0.0.1:5555", "X-Real-IP", "", "10.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			if tc.value != "" {
				h := tc.header
				if h == "" {
					h = "X-Forwarded-For"
				}
				r.Header.Set(h, tc.value)
			}
			if got := ClientIP(r, tc.header); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRulesMatchAPIDoc(t *testing.T) {
	for _, tc := range []struct {
		r      Rule
		limit  int
		closed bool
	}{
		{RuleDefault, 300, false}, {RuleGate, 10, true}, {RuleGateGlobal, 100, true}, {RulePlay, 60, false},
		{RuleAdmin, 30, false},
	} {
		if tc.r.Limit != tc.limit || tc.r.FailClosed != tc.closed || tc.r.Window != time.Minute {
			t.Errorf("rule %s = %+v", tc.r.Name, tc.r)
		}
	}
}
