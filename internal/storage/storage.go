// Package storage wraps S3-compatible object storage (MinIO locally, R2 in production).
// Switching providers is configuration only: the same code talks to both.
package storage

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/vinhpnguyen123/goalstream/internal/config"
)

// Per-call ceilings. Every outbound call is bounded (CLAUDE.md); uploads get the most room
// because a mezzanine is tens of MB and R2 is across the internet.
const (
	putTimeout  = 5 * time.Minute
	listTimeout = 30 * time.Second
)

type Client struct {
	s3      *s3.Client        // internal endpoint: every read and write
	presign *s3.PresignClient // public endpoint: signs URLs, never does I/O
	bucket  string
	ttl     time.Duration
}

func New(c config.S3Config) *Client {
	return &Client{
		s3:      RawClient(c, c.Endpoint),
		presign: s3.NewPresignClient(RawClient(c, c.PublicEndpoint)),
		bucket:  c.Bucket,
		ttl:     c.PresignTTL,
	}
}

// RawClient builds an SDK client for one endpoint. Exported for test helpers only.
//
// The checksum settings matter: recent SDK versions attach CRC checksum headers to every
// PutObject by default, which some S3-compatible stores reject. "When required" sends them
// only for operations whose API demands it.
func RawClient(c config.S3Config, endpoint string) *s3.Client {
	return s3.New(s3.Options{
		Region:                     c.Region, // must be explicit: the SDK refuses to sign without it
		BaseEndpoint:               aws.String(endpoint),
		UsePathStyle:               c.UsePathStyle,
		Credentials:                aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, "")),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

// PutFile uploads a local file to key.
func (c *Client) PutFile(ctx context.Context, key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(ctx, putTimeout)
	defer cancel()
	_, err = c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          f, // an *os.File is seekable, so the SDK can sign the payload
		ContentLength: aws.Int64(st.Size()),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("uploading %s: %w", key, err)
	}
	return nil
}

// PresignGet returns a time-limited GET URL for key, signed for the PUBLIC endpoint.
// Pure computation: no network call. Never log the URL — it is a bearer credential.
func (c *Client) PresignGet(ctx context.Context, key string) (string, time.Time, error) {
	expires := time.Now().Add(c.ttl)
	req, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(c.ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presigning %s: %w", key, err)
	}
	return req.URL, expires, nil
}

// ListKeys returns every key under prefix. Read-only; used by tests and future
// reconciliation — the API never lists the bucket (it reads Postgres).
func (c *Client) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var keys []string
	p := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}
