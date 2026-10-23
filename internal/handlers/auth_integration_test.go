//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
)

const testJWTSecret = "jwt-secret-for-tests-at-least-32-bytes!"

// jwtAPI is setupAPI in AUTH_MODE=jwt: the only mode that mounts /auth/*.
func jwtAPI(t *testing.T) apiEnv {
	t.Helper()
	e := setupAPI(t)
	e.s.Cfg.API.AuthMode = "jwt"
	e.s.Cfg.API.JWTSecret = testJWTSecret
	e.s.Cfg.API.AccessTokenTTL = 15 * time.Minute
	e.s.Cfg.API.RefreshTokenTTL = 168 * time.Hour
	e.s.Cfg.API.RefreshReuseGrace = 30 * time.Second
	e.s.Signer = auth.NewSigner(testJWTSecret, 15*time.Minute)
	// Fresh windows for the auth rules: register counts per HOUR, so without this a re-run
	// (-count=3) inside the hour is rate-limited by the previous one.
	ctx := context.Background()
	for _, rule := range []string{"login", "login_email", "register"} {
		if keys, _ := e.s.Redis.Keys(ctx, "rl:"+rule+":*").Result(); len(keys) > 0 {
			e.s.Redis.Del(ctx, keys...)
		}
	}
	return apiEnv{e.s, e.s.Routes(), e.cookie}
}

// call sends a JSON request from ip with the invite cookie (when gated) and an optional bearer.
func (e apiEnv) call(t *testing.T, method, path, body, ip string, gated bool, bearer string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = ip + ":1234"
	if gated {
		req.AddCookie(e.cookie)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func creds(email, pw string) string {
	return fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)
}

func TestRegister(t *testing.T) {
	e := jwtAPI(t)
	rec, body := e.call(t, "POST", "/api/v1/auth/register",
		`{"email":"  Me@X.com ","password":"correct horse battery","display_name":" Vinh "}`, "203.0.113.9", true, "")
	u, _ := data(body)["user"].(map[string]any)
	if rec.Code != 201 || u["email"] != "me@x.com" || u["display_name"] != "Vinh" || u["id"] == "" {
		t.Fatalf("register: %d %v", rec.Code, body)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("register must not log in")
	}
	if _, ok := u["password_hash"]; ok {
		t.Fatal("the response leaks password_hash")
	}

	for name, c := range map[string]struct {
		body     string
		code     int
		wantType string
	}{
		"taken, other case": {creds("ME@x.com", "another password"), 409, "email_taken"},
		"no @":              {creds("me.x.com", "correct horse battery"), 400, "validation_error"},
		"short password":    {creds("a@x.com", "nine char"), 400, "validation_error"},
		"long name":         {`{"email":"b@x.com","password":"correct horse battery","display_name":"` + strings.Repeat("n", 65) + `"}`, 400, "validation_error"},
		"unknown field":     {`{"email":"c@x.com","password":"correct horse battery","admin":true}`, 400, "validation_error"},
	} {
		// A fresh IP per case: register allows 5 per hour per IP.
		if rec, body := e.call(t, "POST", "/api/v1/auth/register", c.body, "198.51.100."+fmt.Sprint(len(name)), true, ""); rec.Code != c.code || errType(body) != c.wantType {
			t.Errorf("%s: %d %v, want %d %s", name, rec.Code, body, c.code, c.wantType)
		}
	}
	if rec, body := e.call(t, "POST", "/api/v1/auth/register", creds("d@x.com", "correct horse battery"), "203.0.113.9", false, ""); rec.Code != 401 || errType(body) != "gate_required" {
		t.Errorf("no invite cookie: %d %v", rec.Code, body)
	}
}

func TestLogin(t *testing.T) {
	e := jwtAPI(t)
	if rec, body := e.call(t, "POST", "/api/v1/auth/register", creds("me@x.com", "correct horse battery"), "203.0.113.9", true, ""); rec.Code != 201 {
		t.Fatalf("register: %d %v", rec.Code, body)
	}
	rec, body := e.call(t, "POST", "/api/v1/auth/login", creds(" ME@x.com", "correct horse battery"), "203.0.113.9", true, "")
	d := data(body)
	if rec.Code != 200 || d["expires_in"] != float64(900) {
		t.Fatalf("login: %d %v", rec.Code, body)
	}
	user, err := e.s.Signer.Verify(d["access_token"].(string))
	if err != nil {
		t.Fatalf("access token does not verify: %v", err)
	}

	// The refresh cookie: attributes from docs/API.md, and only its hash in the database.
	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.RefreshCookie {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/api/v1/auth" ||
		ck.MaxAge != 604800 || ck.Domain != "" || ck.Secure != e.s.Cfg.API.CookieSecure {
		t.Fatalf("refresh cookie: %+v", ck)
	}
	var owner string
	var revoked bool
	if err := e.s.Pool.QueryRow(context.Background(), `
		SELECT user_id, revoked_at IS NOT NULL FROM refresh_tokens
		 WHERE token_hash = $1 AND expires_at > now() + interval '167 hours'`, auth.HashRefresh(ck.Value)).Scan(&owner, &revoked); err != nil {
		t.Fatalf("refresh row: %v", err)
	}
	if owner != user || revoked {
		t.Fatalf("refresh row owner %s revoked %v, want %s and active", owner, revoked, user)
	}
	if n := e.count(t, `SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, ck.Value); n != 0 {
		t.Fatal("the raw refresh token is stored")
	}

	// The bearer opens user routes; the gate still answers first without the invite cookie.
	if rec, _ := e.call(t, "GET", "/api/v1/clips", "", "203.0.113.9", true, d["access_token"].(string)); rec.Code != 200 {
		t.Errorf("GET /clips with bearer: %d", rec.Code)
	}
	if rec, body := e.call(t, "GET", "/api/v1/clips", "", "203.0.113.9", false, d["access_token"].(string)); rec.Code != 401 || errType(body) != "gate_required" {
		t.Errorf("bearer without invite cookie: %d %v", rec.Code, body)
	}
	if rec, body := e.call(t, "GET", "/api/v1/clips", "", "203.0.113.9", true, ""); rec.Code != 401 || errType(body) != "unauthorized" {
		t.Errorf("no bearer: %d %v", rec.Code, body)
	}

	// Wrong password and unknown email are indistinguishable to the caller.
	_, wrong := e.call(t, "POST", "/api/v1/auth/login", creds("me@x.com", "wrong horse battery"), "203.0.113.9", true, "")
	_, unknown := e.call(t, "POST", "/api/v1/auth/login", creds("nobody@x.com", "correct horse battery"), "203.0.113.9", true, "")
	delete(wrong, "request_id")
	delete(unknown, "request_id")
	if errType(wrong) != "invalid_credentials" || fmt.Sprint(wrong) != fmt.Sprint(unknown) {
		t.Errorf("wrong password %v vs unknown email %v", wrong, unknown)
	}
}

// login_email counts per email whatever the IP; every auth rule fails closed (§12 #12 shape).
func TestLoginLimitsAndRedisDown(t *testing.T) {
	e := jwtAPI(t)
	// Fixed windows reset on the minute: six argon2id logins that straddle the boundary land
	// in two windows and none is limited. That is the limiter working as designed (README M1),
	// so a run that crossed the boundary is retried with a fresh email instead of failing.
	var codes []int
	checked := false
	for try := 0; try < 3 && !checked; try++ {
		window := time.Now().Unix() / 60
		email := fmt.Sprintf("target%d@x.com", try)
		codes = codes[:0]
		var last *httptest.ResponseRecorder
		for i := 1; i <= 6; i++ {
			last, _ = e.call(t, "POST", "/api/v1/auth/login", creds(email, "wrong horse battery"), fmt.Sprintf("198.51.100.%d", i), true, "")
			codes = append(codes, last.Code)
		}
		if time.Now().Unix()/60 != window {
			continue
		}
		if fmt.Sprint(codes) != "[401 401 401 401 401 429]" || last.Header().Get("Retry-After") == "" {
			t.Fatalf("six logins for one email from six IPs: %v (Retry-After %q), want five 401 then 429",
				codes, last.Header().Get("Retry-After"))
		}
		checked = true
	}
	if !checked {
		t.Fatal("every try straddled a minute boundary; nothing was checked")
	}

	e.s.Redis = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	down := apiEnv{e.s, e.s.Routes(), e.cookie}
	for _, p := range []string{"/api/v1/auth/login", "/api/v1/auth/register"} {
		if rec, body := down.call(t, "POST", p, creds("me@x.com", "correct horse battery"), "203.0.113.9", true, ""); rec.Code != 503 || errType(body) != "unavailable" {
			t.Errorf("%s with Redis down: %d %v, want 503", p, rec.Code, body)
		}
	}
}

// dev and closed have no JWT_SECRET: /auth/* is not mounted there at all.
func TestAuthRoutesOnlyInJWTMode(t *testing.T) {
	e := setupAPI(t)
	for _, mode := range []string{"dev", "closed"} {
		e.s.Cfg.API.AuthMode = mode
		h := apiEnv{e.s, e.s.Routes(), e.cookie}
		for _, p := range []string{"/api/v1/auth/login", "/api/v1/auth/register"} {
			if rec, body := h.call(t, "POST", p, creds("me@x.com", "correct horse battery"), "203.0.113.9", true, ""); rec.Code != 404 {
				t.Errorf("%s in %s mode: %d %v, want 404", p, mode, rec.Code, body)
			}
		}
	}
}
