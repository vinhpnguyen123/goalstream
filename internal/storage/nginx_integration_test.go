//go:build integration

package storage_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/storage/storagetest"
)

// §12 #38, the local nginx cache (deploy/local/nginx): the same segment is a MISS then a HIT,
// the query string is not part of the key, a non-200 is never cached, X-Cache-Status is
// exposed, CORS names the configured origin whatever the request's, and nothing outside
// clips/*/hls/* is proxied.
func TestNginxCache(t *testing.T) {
	ctx := context.Background()
	cfg := storagetest.Config(t)
	c := storage.New(cfg.S3)
	u, err := url.Parse(os.Getenv("HLS_BASE_URL"))
	if err != nil || u.Host == "" {
		t.Fatalf("HLS_BASE_URL must point at the local nginx (make up), got %q", os.Getenv("HLS_BASE_URL"))
	}
	base := u.Scheme + "://" + u.Host + "/" + cfg.S3.Bucket + "/"
	// Fresh keys every run: the cache outlives the per-test bucket wipe.
	clip := "clips/" + uuid.NewString() + "/"
	prefix := clip + "hls/" + uuid.NewString() + "/"
	cookie := storagetest.InviteCookie(t)

	put := func(key, body string) {
		t.Helper()
		f := filepath.Join(t.TempDir(), "obj")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := c.PutFile(ctx, key, f, "video/mp2t", ""); err != nil {
			t.Fatal(err)
		}
	}
	type resp struct {
		status      int
		cache, body string
		h           http.Header
	}
	do := func(method, path, origin string, hdr ...string) resp {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.Header.Set("Cookie", cookie) // M9: nginx admits only the invite (TestNginxInvite)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp{res.StatusCode, res.Header.Get("X-Cache-Status"), string(b), res.Header}
	}
	want := func(r resp, status int, cache string) {
		t.Helper()
		if r.status != status || r.cache != cache {
			t.Errorf("got %d %q, want %d %q", r.status, r.cache, status, cache)
		}
	}

	seg := prefix + "720p/seg_00000.ts"
	put(seg, "segment bytes")
	want(do("GET", seg, ""), 200, "MISS")
	if r := do("GET", seg, ""); r.body != "segment bytes" {
		t.Errorf("HIT body = %q", r.body)
	} else {
		want(r, 200, "HIT")
	}
	// A presign signature in the query must not fragment the key.
	want(do("GET", seg+"?X-Amz-Signature=junk", ""), 200, "HIT")
	// Any Origin — MinIO's Vary: Origin is ignored, so it is the same entry, and CORS names the
	// configured origin, never the caller's and never *.
	r := do("GET", seg, "http://evil.example")
	want(r, 200, "HIT")
	if a := r.h.Get("Access-Control-Allow-Origin"); a == "" || a == "*" || a == "http://evil.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the configured origin", a)
	}
	if !strings.Contains(r.h.Get("Access-Control-Expose-Headers"), "X-Cache-Status") {
		t.Errorf("X-Cache-Status not exposed to the page: %q", r.h.Get("Access-Control-Expose-Headers"))
	}

	// MinIO admits hls/ only from nginx's address and takes that address from forwarding headers
	// when present (M9): nginx must not pass the client's on, or a cold read would be refused.
	fwd := prefix + "720p/seg_00002.ts"
	put(fwd, "fwd")
	want(do("GET", fwd, "", "X-Forwarded-For", "203.0.113.9", "X-Real-IP", "203.0.113.9",
		"Forwarded", "for=203.0.113.9"), 200, "MISS")

	// Never cache a non-200: a segment that is missing now is served the moment it exists.
	late := prefix + "720p/seg_00001.ts"
	want(do("GET", late, ""), 404, "MISS")
	want(do("GET", late, ""), 404, "MISS")
	put(late, "late")
	want(do("GET", late, ""), 200, "MISS")

	// Read-only, and only clips/*/hls/*: originals are never reachable through the cache.
	want(do("POST", seg, ""), 403, "")
	put(clip+"original.mp4", "mezzanine")
	want(do("GET", clip+"original.mp4", ""), 404, "")
}

// §12 #39, nginx half: the invite cookie is the credential for clips/*/hls/* — checked before
// the cache, so a cached segment is refused too; every way a cookie can be wrong is a 403; the
// cookie never reaches MinIO's side of the log, and neither does a query string.
// (MinIO half — a bare URL straight to MinIO is 403 — is TestLocalHLSExposure.)
func TestNginxInvite(t *testing.T) {
	ctx := context.Background()
	cfg := storagetest.Config(t)
	u, err := url.Parse(os.Getenv("HLS_BASE_URL"))
	if err != nil || u.Host == "" {
		t.Fatalf("HLS_BASE_URL must point at the local nginx (make up), got %q", os.Getenv("HLS_BASE_URL"))
	}
	seg := "clips/" + uuid.NewString() + "/hls/" + uuid.NewString() + "/720p/seg_00000.ts"
	f := filepath.Join(t.TempDir(), "obj")
	if err := os.WriteFile(f, []byte("segment"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storage.New(cfg.S3).PutFile(ctx, seg, f, "video/mp2t", ""); err != nil {
		t.Fatal(err)
	}
	marker := uuid.NewString() // in the query string; must never reach the log
	target := u.Scheme + "://" + u.Host + "/" + cfg.S3.Bucket + "/" + seg + "?X-Amz-Signature=" + marker
	get := func(cookie string) (int, string, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode, res.Header.Get("X-Cache-Status"), res.Header
	}
	enc := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	secret, id := cfg.API.InviteSecret, ""
	for k := range cfg.API.InviteCodes {
		id = k
	}
	hour := time.Now().Add(time.Hour)
	valid := auth.SignInvite(secret, id, hour)
	sig := strings.Split(mustDecode(t, valid), "|")[2]

	// Warm the cache with a valid cookie first: every refusal below is then of a CACHED object.
	if code, cache, h := get("gs_invite=" + valid); code != 200 || cache != "MISS" {
		t.Fatalf("valid cookie: %d %q, want 200 MISS", code, cache)
	} else if h.Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true (hls.js sends credentials)", h.Get("Access-Control-Allow-Credentials"))
	}
	for name, cookie := range map[string]string{
		"no cookie":         "",
		"empty":             "gs_invite=",
		"other secret":      "gs_invite=" + auth.SignInvite(strings.Repeat("x", 40), id, hour),
		"MAC flipped":       "gs_invite=" + enc(id+"|"+strconv.FormatInt(hour.Unix(), 10)+"|"+flip(sig)),
		"exp extended":      "gs_invite=" + enc(id+"|"+strconv.FormatInt(hour.Unix()+3600, 10)+"|"+sig),
		"expired":           "gs_invite=" + auth.SignInvite(secret, id, time.Now().Add(-time.Second)),
		"revoked id":        "gs_invite=" + auth.SignInvite(secret, "not-configured", hour),
		"two parts":         "gs_invite=" + enc(id+"|"+sig),
		"not base64url":     "gs_invite=" + valid + "!",
		"padded base64":     "gs_invite=" + base64.URLEncoding.EncodeToString([]byte(mustDecode(t, valid))) + "=",
		"other cookie name": "gs_invitex=" + valid,
	} {
		if code, cache, h := get(cookie); code != 403 || cache != "" {
			t.Errorf("%s: %d %q, want 403 before the cache", name, code, cache)
		} else if h.Get("Access-Control-Allow-Origin") == "" {
			t.Errorf("%s: 403 without CORS headers — hls.js could not read the status", name)
		}
	}
	// Among other cookies, as a browser sends it.
	if code, cache, _ := get("a=1; gs_invite=" + valid + "; b=2"); code != 200 || cache != "HIT" {
		t.Errorf("valid cookie among others: %d %q, want 200 HIT", code, cache)
	}

	// The log never carries the query string or the cookie.
	out, err := exec.CommandContext(ctx, "docker", "compose", "-f", "../../docker-compose.yml",
		"logs", "--no-log-prefix", "--since", "2m", "nginx").CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose logs: %v: %s", err, out)
	}
	if !strings.Contains(string(out), seg) {
		t.Fatalf("nginx log has no line for %s — reading the wrong container?", seg)
	}
	for _, secretish := range []string{marker, "X-Amz-Signature", valid, sig} {
		if strings.Contains(string(out), secretish) {
			t.Errorf("nginx log contains %q", secretish)
		}
	}
}

func mustDecode(t *testing.T, v string) string {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// flip changes the last hex digit: a MAC of the right length and alphabet that is wrong.
func flip(hex string) string {
	last := hex[len(hex)-1]
	if last == '0' {
		return hex[:len(hex)-1] + "1"
	}
	return hex[:len(hex)-1] + "0"
}
