//go:build integration

package middleware

import (
	"context"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// §12 #12 (first half): N requests pass, N+1 gets 429 with Retry-After.
func TestRateLimitBlocksAfterLimit(t *testing.T) {
	opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { rdb.Close() })
	// A unique rule name isolates this run's keys; an hour window keeps a minute boundary
	// from resetting the count mid-test.
	rule := Rule{Name: "test-" + uuid.NewString(), Limit: 3, Window: time.Hour}
	h := RateLimit(rdb, rule, ByIP(""))(ok)

	call := func(remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	for i := range 3 {
		if rec := call("203.0.113.1:1"); rec.Code != 204 {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
	}
	rec := call("203.0.113.1:1")
	if rec.Code != 429 {
		t.Fatalf("request 4: %d, want 429", rec.Code)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 3600 {
		t.Fatalf("Retry-After %q, want seconds to the window end", rec.Header().Get("Retry-After"))
	}
	if rec := call("203.0.113.2:1"); rec.Code != 204 {
		t.Fatalf("another IP was limited: %d", rec.Code)
	}

	keys, _ := rdb.Keys(context.Background(), "rl:"+rule.Name+":*").Result()
	for _, k := range keys {
		if ttl := rdb.TTL(context.Background(), k).Val(); ttl <= 0 || ttl > time.Hour {
			t.Errorf("key %s has TTL %v: windows must expire", k, ttl)
		}
		rdb.Del(context.Background(), k)
	}
}
