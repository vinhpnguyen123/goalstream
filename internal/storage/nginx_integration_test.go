//go:build integration

package storage_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

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
