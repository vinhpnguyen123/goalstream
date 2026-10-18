// Package config loads all configuration from environment variables once, at startup, and
// fails fast — listing every problem at once rather than one per restart.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppEnv      string // dev | test | prod
	LogLevel    string
	DatabaseURL string
	RedisURL    string
	S3          S3Config
	Ingest      IngestConfig
}

type S3Config struct {
	Endpoint       string // what our processes reach
	PublicEndpoint string // what browsers reach; the only endpoint that signs URLs
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	UsePathStyle   bool
	PresignTTL     time.Duration
}

type IngestConfig struct {
	DataDir      string // required by `ingest run` only, checked there
	Labels       []string
	PreRoll      time.Duration
	PostRoll     time.Duration
	MinClip      time.Duration
	CRF          int
	MaxRate      int // absolute ceiling, bits/sec; the effective cap is min(source, this) (D15)
	CutTimeout   time.Duration
	TagsTodoPath string
}

// Load reads the environment through getenv (os.Getenv in production, a map in tests).
func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	c := Config{
		AppEnv:      p.oneOf("APP_ENV", "", "dev", "test", "prod"),
		LogLevel:    p.str("LOG_LEVEL", "info"),
		DatabaseURL: p.required("DATABASE_URL"),
		RedisURL:    p.required("REDIS_URL"),
		S3: S3Config{
			Endpoint:       p.required("S3_ENDPOINT"),
			PublicEndpoint: p.required("S3_PUBLIC_ENDPOINT"),
			Region:         p.required("S3_REGION"),
			Bucket:         p.required("S3_BUCKET"),
			AccessKey:      p.required("S3_ACCESS_KEY"),
			SecretKey:      p.required("S3_SECRET_KEY"),
			UsePathStyle:   p.boolean("S3_USE_PATH_STYLE", true),
			PresignTTL:     p.duration("PRESIGN_TTL", 15*time.Minute),
		},
		Ingest: IngestConfig{
			DataDir:      p.str("DATA_DIR", ""),
			Labels:       p.list("INGEST_LABELS", "Goal"),
			PreRoll:      p.seconds("PRE_ROLL_SECONDS", 15),
			PostRoll:     p.seconds("POST_ROLL_SECONDS", 45),
			MinClip:      p.seconds("MIN_CLIP_SECONDS", 30),
			CRF:          p.integer("MEZZ_CRF", 18),
			MaxRate:      p.integer("MEZZ_MAXRATE", 8_000_000),
			CutTimeout:   p.duration("INGEST_CUT_TIMEOUT", 5*time.Minute),
			TagsTodoPath: p.str("TAGS_TODO_PATH", "./tags_todo.csv"),
		},
	}
	if c.AppEnv == "prod" {
		if err := publicEndpointGuard(c.S3.PublicEndpoint); err != nil {
			p.errs = append(p.errs, err)
		}
	}
	return c, errors.Join(p.errs...)
}

// publicEndpointGuard rejects endpoints a browser on the internet cannot reach. A URL signed
// for localhost or a docker-internal hostname works locally and fails in production — the
// most likely deployment failure in this project.
func publicEndpointGuard(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("S3_PUBLIC_ENDPOINT %q is not a URL", endpoint)
	}
	h := u.Hostname()
	if h == "localhost" || !strings.Contains(h, ".") {
		return fmt.Errorf("S3_PUBLIC_ENDPOINT host %q is not publicly reachable", h)
	}
	if ip := net.ParseIP(h); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified()) {
		return fmt.Errorf("S3_PUBLIC_ENDPOINT host %q is not publicly reachable", h)
	}
	return nil
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) str(key, def string) string {
	if v := strings.TrimSpace(p.getenv(key)); v != "" {
		return v
	}
	return def
}

func (p *parser) required(key string) string {
	v := p.str(key, "")
	if v == "" {
		p.errs = append(p.errs, fmt.Errorf("%s is required", key))
	}
	return v
}

func (p *parser) oneOf(key, def string, allowed ...string) string {
	v := p.str(key, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.errs = append(p.errs, fmt.Errorf("%s must be one of %v, got %q", key, allowed, v))
	return v
}

func (p *parser) integer(key string, def int) int {
	raw := p.str(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s must be an integer, got %q", key, raw))
	}
	return n
}

func (p *parser) seconds(key string, def int) time.Duration {
	return time.Duration(p.integer(key, def)) * time.Second
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	raw := p.str(key, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s must be a duration like 15m, got %q", key, raw))
	}
	return d
}

func (p *parser) boolean(key string, def bool) bool {
	raw := p.str(key, "")
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s must be true or false, got %q", key, raw))
	}
	return b
}

func (p *parser) list(key, def string) []string {
	var out []string
	for _, s := range strings.Split(p.str(key, def), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
