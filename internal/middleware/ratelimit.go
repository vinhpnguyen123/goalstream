package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// Rule is one row of docs/API.md § Rate limits. Limits are constants, not env vars.
type Rule struct {
	Name       string
	Limit      int
	Window     time.Duration
	FailClosed bool // brute-force protection must not vanish when Redis does
}

var (
	RuleDefault    = Rule{"default", 300, time.Minute, false}
	RuleGate       = Rule{"gate", 10, time.Minute, true}
	RuleGateGlobal = Rule{"gate_global", 100, time.Minute, true}
	RulePlay       = Rule{"play", 60, time.Minute, false}
)

// storeTimeout bounds every Redis call: a slow limiter must not become a slow API.
const storeTimeout = 250 * time.Millisecond

// KeyFunc picks what a rule counts per: an IP, a user, or one global bucket.
type KeyFunc func(*http.Request) string

func ByIP(header string) KeyFunc { return func(r *http.Request) string { return ClientIP(r, header) } }

func Global(*http.Request) string { return "all" }

// ClientIP is RemoteAddr, or the first entry of header when the API sits behind a proxy that
// overwrites it (CLIENT_IP_HEADER). Trusting a header the proxy only appends to would let any
// client pick its own rate-limit key.
func ClientIP(r *http.Request, header string) string {
	if header != "" {
		if v := r.Header.Get(header); v != "" {
			first, _, _ := strings.Cut(v, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimit is a fixed window: INCR rl:<rule>:<key>:<window>, EXPIRE on the same key.
func RateLimit(rdb *redis.Client, rule Rule, key KeyFunc) func(http.Handler) http.Handler {
	secs := int64(rule.Window / time.Second)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			now := time.Now().Unix()
			k := fmt.Sprintf("rl:%s:%s:%d", rule.Name, key(r), now/secs)
			n, err := hit(r.Context(), rdb, k, rule.Window)
			if err != nil {
				if rule.FailClosed {
					httpx.Log(r.Context()).Error("rate limit store down; failing closed", "rule", rule.Name, "err", err)
					httpx.Error(w, r, http.StatusServiceUnavailable, "unavailable", "temporarily unavailable, try again shortly")
					return
				}
				httpx.Log(r.Context()).Warn("rate limit store down; failing open", "rule", rule.Name, "err", err)
				next.ServeHTTP(w, r)
				return
			}
			if n > int64(rule.Limit) {
				w.Header().Set("Retry-After", strconv.FormatInt(secs-now%secs, 10))
				httpx.Error(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hit(ctx context.Context, rdb *redis.Client, key string, ttl time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	pipe := rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}
