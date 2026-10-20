// Package storagetest gives integration tests a clean, dedicated test bucket. Imported by
// tests only — production code never deletes objects (the mezzanine is never deleted).
package storagetest

import (
	"context"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
)

// Config loads the environment and points it at S3_TEST_BUCKET, which is emptied first.
func Config(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.S3.Bucket = os.Getenv("S3_TEST_BUCKET")
	if cfg.S3.Bucket == "" || cfg.S3.Bucket == os.Getenv("S3_BUCKET") {
		t.Fatal("S3_TEST_BUCKET must be set and differ from S3_BUCKET")
	}
	Empty(t, cfg.S3)
	return cfg
}

// Empty deletes every object in the configured bucket.
func Empty(t *testing.T, c config.S3Config) {
	t.Helper()
	ctx := context.Background()
	cl := storage.RawClient(c, c.Endpoint)
	p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{Bucket: aws.String(c.Bucket)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatalf("listing test bucket: %v", err)
		}
		for _, o := range page.Contents {
			if _, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.Bucket), Key: o.Key}); err != nil {
				t.Fatalf("emptying test bucket: %v", err)
			}
		}
	}
}
