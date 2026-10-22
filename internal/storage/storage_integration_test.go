//go:build integration

package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/storage/storagetest"
)

func TestPutPresignGetAndRange(t *testing.T) {
	ctx := context.Background()
	cfg := storagetest.Config(t)
	c := storage.New(cfg.S3)

	body := bytes.Repeat([]byte("0123456789"), 1000) // 10,000 bytes
	path := filepath.Join(t.TempDir(), "original.mp4")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	key := "clips/test-clip/original.mp4"
	if err := c.PutFile(ctx, key, path, "video/mp4"); err != nil {
		t.Fatal(err)
	}

	keys, err := c.ListKeys(ctx, "clips/")
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("ListKeys = %v, %v", keys, err)
	}

	signed, expires, err := c.PresignGet(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expires); d < cfg.S3.PresignTTL-time.Minute || d > cfg.S3.PresignTTL {
		t.Fatalf("expires in %v, want about %v", d, cfg.S3.PresignTTL)
	}
	// Signed for the PUBLIC endpoint, never the internal one.
	u, _ := url.Parse(signed)
	pub, _ := url.Parse(cfg.S3.PublicEndpoint)
	if u.Host != pub.Host {
		t.Fatalf("signed for host %q, want public host %q", u.Host, pub.Host)
	}

	// Full GET straight from storage — the bytes never pass through our code.
	resp, err := http.Get(signed)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, body) || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("GET: status %d, %d bytes, type %q", resp.StatusCode, len(got), resp.Header.Get("Content-Type"))
	}

	// Range request (§12 #9): storage answers 206 natively, which is why seeking needs no API.
	req, _ := http.NewRequest(http.MethodGet, signed, nil)
	req.Header.Set("Range", "bytes=100-109")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(got) != "0123456789" ||
		resp.Header.Get("Content-Range") != "bytes 100-109/10000" {
		t.Fatalf("Range: status %d, body %q, Content-Range %q", resp.StatusCode, got, resp.Header.Get("Content-Range"))
	}

	// Tampering with the signature must fail: the URL is a capability, not a hint.
	q := u.Query()
	q.Set("X-Amz-Signature", "0000"+q.Get("X-Amz-Signature")[4:])
	u.RawQuery = q.Encode()
	resp, err = http.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered signature: status %d, want 403", resp.StatusCode)
	}
}

func TestExpiredPresignIsRejected(t *testing.T) {
	ctx := context.Background()
	cfg := storagetest.Config(t)
	cfg.S3.PresignTTL = time.Second
	c := storage.New(cfg.S3)

	path := filepath.Join(t.TempDir(), "x.mp4")
	os.WriteFile(path, []byte("x"), 0o600)
	if err := c.PutFile(ctx, "clips/x/original.mp4", path, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	signed, _, err := c.PresignGet(ctx, "clips/x/original.mp4")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	resp, err := http.Get(signed)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired URL: status %d, want 403", resp.StatusCode)
	}
}

func TestObjectsAreNotPublic(t *testing.T) {
	ctx := context.Background()
	cfg := storagetest.Config(t)
	c := storage.New(cfg.S3)
	path := filepath.Join(t.TempDir(), "x.mp4")
	os.WriteFile(path, []byte("x"), 0o600)
	if err := c.PutFile(ctx, "clips/y/original.mp4", path, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(cfg.S3.PublicEndpoint + "/" + cfg.S3.Bucket + "/clips/y/original.mp4")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned GET: status %d, want 403 (originals are never public)", resp.StatusCode)
	}
}

// Open reads through the internal client — the M3 stream handler's only way to the bytes.
func TestOpenWholeRangedAndMissing(t *testing.T) {
	ctx := context.Background()
	cfg := storagetest.Config(t)
	c := storage.New(cfg.S3)
	body := bytes.Repeat([]byte("0123456789"), 100)
	path := filepath.Join(t.TempDir(), "o.mp4")
	os.WriteFile(path, body, 0o600)
	if err := c.PutFile(ctx, "clips/o/original.mp4", path, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	read := func(rng string) []byte {
		t.Helper()
		rc, err := c.Open(ctx, "clips/o/original.mp4", rng)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(""); !bytes.Equal(got, body) {
		t.Fatalf("whole: %d bytes, want %d", len(got), len(body))
	}
	if got := read("bytes=13-22"); string(got) != "3456789012" {
		t.Fatalf("ranged: %q", got)
	}
	if _, err := c.Open(ctx, "clips/missing/original.mp4", ""); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing key: %v, want ErrNotFound", err)
	}
}
