package config

import (
	"strings"
	"testing"
	"time"
)

func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		"APP_ENV":            "dev",
		"DATABASE_URL":       "postgres://u:p@127.0.0.1:5442/goalstream",
		"REDIS_URL":          "redis://127.0.0.1:6380/0",
		"S3_ENDPOINT":        "http://127.0.0.1:9010",
		"S3_PUBLIC_ENDPOINT": "http://127.0.0.1:9010",
		"S3_REGION":          "us-east-1",
		"S3_BUCKET":          "goalstream",
		"S3_ACCESS_KEY":      "k",
		"S3_SECRET_KEY":      "s",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ingest.PreRoll != 15*time.Second || c.Ingest.PostRoll != 45*time.Second ||
		c.Ingest.MinClip != 30*time.Second || c.Ingest.CRF != 18 || c.Ingest.MaxRate != 8_000_000 {
		t.Fatalf("ingest defaults wrong: %+v", c.Ingest)
	}
	if c.S3.PresignTTL != 15*time.Minute || !c.S3.UsePathStyle {
		t.Fatalf("s3 defaults wrong: %+v", c.S3)
	}
	if len(c.Ingest.Labels) != 1 || c.Ingest.Labels[0] != "Goal" {
		t.Fatalf("labels default wrong: %v", c.Ingest.Labels)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr []string // every substring must appear: all problems are reported at once
	}{
		{"missing required listed together", map[string]string{"DATABASE_URL": "", "S3_REGION": ""},
			[]string{"DATABASE_URL is required", "S3_REGION is required"}},
		{"bad app env", map[string]string{"APP_ENV": "staging"}, []string{"APP_ENV must be one of"}},
		{"missing app env", map[string]string{"APP_ENV": ""}, []string{"APP_ENV must be one of"}},
		{"bad integer", map[string]string{"MEZZ_CRF": "eighteen"}, []string{"MEZZ_CRF must be an integer"}},
		{"bad duration", map[string]string{"PRESIGN_TTL": "15"}, []string{"PRESIGN_TTL must be a duration"}},
		{"bad bool", map[string]string{"S3_USE_PATH_STYLE": "yes please"}, []string{"S3_USE_PATH_STYLE must be"}},
		{"prod localhost endpoint", map[string]string{"APP_ENV": "prod", "S3_PUBLIC_ENDPOINT": "http://localhost:9010"},
			[]string{"not publicly reachable"}},
		{"prod loopback ip", map[string]string{"APP_ENV": "prod"}, []string{"not publicly reachable"}},
		{"prod private ip", map[string]string{"APP_ENV": "prod", "S3_PUBLIC_ENDPOINT": "http://10.0.0.5:9000"},
			[]string{"not publicly reachable"}},
		{"prod docker hostname", map[string]string{"APP_ENV": "prod", "S3_PUBLIC_ENDPOINT": "http://minio:9000"},
			[]string{"not publicly reachable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestProdAcceptsPublicEndpoint(t *testing.T) {
	_, err := Load(env(map[string]string{
		"APP_ENV":            "prod",
		"S3_PUBLIC_ENDPOINT": "https://abc123.r2.cloudflarestorage.com",
	}))
	if err != nil {
		t.Fatalf("public R2 endpoint rejected: %v", err)
	}
}

func TestLabelsTrimmed(t *testing.T) {
	c, err := Load(env(map[string]string{"INGEST_LABELS": " Goal , Yellow card ,,"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Ingest.Labels) != 2 || c.Ingest.Labels[1] != "Yellow card" {
		t.Fatalf("labels: %q", c.Ingest.Labels)
	}
}
