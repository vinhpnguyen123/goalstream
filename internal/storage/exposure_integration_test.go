//go:build integration

package storage_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/storage/storagetest"
)

// §12 #28, local exposure. deploy/local/hls-read-policy.json lets anything on this machine read
// clips/*/hls/* without a signature (the HLS player's relative requests carry none); originals,
// everything else and listing stay private; and MinIO answers on 127.0.0.1 only.
func TestLocalHLSExposure(t *testing.T) {
	ctx := context.Background()
	cfg := storagetest.Config(t)
	c := storage.New(cfg.S3)
	f := filepath.Join(t.TempDir(), "obj")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"clips/c1/original.mp4", "clips/c1/hls/v1/master.m3u8",
		"clips/c1/hls/v1/720p/seg_00000.ts", "clips/c1/other.txt", "hls/v1/master.m3u8"} {
		if err := c.PutFile(ctx, k, f, "application/octet-stream", ""); err != nil {
			t.Fatal(err)
		}
	}
	base := cfg.S3.Endpoint + "/" + cfg.S3.Bucket + "/"
	for key, want := range map[string]int{
		"clips/c1/hls/v1/master.m3u8":       200,
		"clips/c1/hls/v1/720p/seg_00000.ts": 200,
		"clips/c1/original.mp4":             403, // presigned only, ever
		"clips/c1/other.txt":                403,
		"hls/v1/master.m3u8":                403, // the policy is scoped to clips/*/hls/*
		"?list-type=2":                      403, // no anonymous listing
	} {
		res, err := http.Get(base + key)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("unsigned GET %s = %d, want %d", key, res.StatusCode, want)
		}
	}

	// Bound to loopback: the same port on any other address of this machine refuses.
	u, err := url.Parse(cfg.S3.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	tried := 0
	for _, a := range addrs {
		ip, ok := a.(*net.IPNet)
		if !ok || ip.IP.IsLoopback() || ip.IP.To4() == nil || ip.IP.IsLinkLocalUnicast() {
			continue
		}
		tried++
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.IP.String(), u.Port()), 2*time.Second)
		if err == nil {
			conn.Close()
			t.Errorf("MinIO is reachable on %s:%s — it must bind 127.0.0.1 only", ip.IP, u.Port())
		} else if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Logf("%s: %v", ip.IP, err) // filtered rather than refused: still unreachable
		}
	}
	if tried == 0 {
		t.Log("no non-loopback IPv4 address on this machine; the bind check had nothing to probe")
	}
}
