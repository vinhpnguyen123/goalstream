// Package config loads all configuration from environment variables once, at startup, and
// fails fast — listing every problem at once rather than one per restart.
package config

import (
	"errors"
	"fmt"
	"math"
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
	API         APIConfig
	Reco        RecoConfig
}

// RecoConfig tunes selection (plan §11). Read by the API only.
type RecoConfig struct {
	ExploreRate        float64 // share of warm picks served from below the median similarity
	ColdStartThreshold int     // liked + disliked below this → uniform random, no centroid query
	TopK               int     // exploit stage 1 candidates, re-ranked in Go
	NegPenalty         float64 // weight of cos(v, negative) in the stage 2 score
}

// APIConfig is read by every binary but only cmd/api requires it: ValidateAPI.
type APIConfig struct {
	HTTPAddr     string
	AuthMode     string // dev | closed | jwt
	DevUserEmail string
	// AUTH_MODE=jwt only. Access tokens are verified with no leeway: one process signs and
	// verifies them, so there is no clock skew to absorb.
	JWTSecret         string
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	RefreshReuseGrace time.Duration // a rotated token replayed within this is a tab race, not theft
	CookieSecure      bool
	GateEnabled       bool
	InviteCodes       map[string]string // invite id → code; removing an id revokes its cookies
	InviteSecret      string
	InviteTTL         time.Duration
	ClientIPHeader    string // set only behind a proxy that overwrites it
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
	MaxRate      int // absolute ceiling, bits/sec; the effective cap is MezzanineCap (D15)
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
		API: APIConfig{
			HTTPAddr:          p.str("HTTP_ADDR", ":8080"),
			AuthMode:          p.str("AUTH_MODE", ""),
			DevUserEmail:      strings.ToLower(p.str("DEV_USER_EMAIL", "dev@localhost")),
			JWTSecret:         p.str("JWT_SECRET", ""),
			AccessTokenTTL:    p.duration("ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL:   p.duration("REFRESH_TOKEN_TTL", 168*time.Hour),
			RefreshReuseGrace: p.duration("REFRESH_REUSE_GRACE", 30*time.Second),
			CookieSecure:      p.boolean("COOKIE_SECURE", true),
			GateEnabled:       p.boolean("GATE_ENABLED", true),
			InviteCodes:       p.inviteCodes("INVITE_CODES"),
			InviteSecret:      p.str("INVITE_SECRET", ""),
			InviteTTL:         p.duration("INVITE_TTL", 720*time.Hour),
			ClientIPHeader:    p.str("CLIENT_IP_HEADER", ""),
		},
		Reco: RecoConfig{
			ExploreRate:        p.float("EXPLORE_RATE", 0.3),
			ColdStartThreshold: p.integer("COLD_START_THRESHOLD", 5),
			TopK:               p.integer("RECO_TOP_K", 50),
			NegPenalty:         p.float("NEG_PENALTY", 0.5),
		},
	}
	if r := c.Reco; r.ExploreRate < 0 || r.ExploreRate > 1 || r.ColdStartThreshold < 0 ||
		r.TopK < 1 || r.TopK > 1000 || r.NegPenalty < 0 {
		p.errs = append(p.errs, errors.New("EXPLORE_RATE must be in [0,1], COLD_START_THRESHOLD ≥ 0, RECO_TOP_K 1–1000, NEG_PENALTY ≥ 0"))
	}
	// Startup guards (CONVENTIONS.md): settings that are only safe on a laptop.
	if c.AppEnv == "prod" {
		if err := publicEndpointGuard(c.S3.PublicEndpoint); err != nil {
			p.errs = append(p.errs, err)
		}
		if c.API.AuthMode == "dev" {
			p.errs = append(p.errs, errors.New("AUTH_MODE=dev is refused in prod"))
		}
		if !c.API.GateEnabled {
			p.errs = append(p.errs, errors.New("GATE_ENABLED=false is refused in prod"))
		}
	}
	if c.AppEnv != "dev" && !c.API.CookieSecure {
		p.errs = append(p.errs, errors.New("COOKIE_SECURE=false is refused outside APP_ENV=dev"))
	}
	return c, errors.Join(p.errs...)
}

// ValidateAPI checks what only the API needs, so ingest runs without gate secrets.
func (c Config) ValidateAPI() error {
	var errs []error
	switch c.API.AuthMode {
	case "dev", "closed":
	case "jwt":
		if len(c.API.JWTSecret) < 32 {
			errs = append(errs, errors.New("JWT_SECRET must be at least 32 bytes when AUTH_MODE=jwt"))
		}
	case "":
		errs = append(errs, errors.New("AUTH_MODE is required"))
	default:
		errs = append(errs, fmt.Errorf("AUTH_MODE must be dev, closed or jwt, got %q", c.API.AuthMode))
	}
	if c.API.GateEnabled {
		if len(c.API.InviteCodes) == 0 {
			errs = append(errs, errors.New("INVITE_CODES is required when the gate is enabled"))
		}
		if len(c.API.InviteSecret) < 32 {
			errs = append(errs, errors.New("INVITE_SECRET must be at least 32 bytes"))
		}
	}
	return errors.Join(errs...)
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

func (p *parser) float(key string, def float64) float64 {
	raw := p.str(key, "")
	if raw == "" {
		return def
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		p.errs = append(p.errs, fmt.Errorf("%s must be a number, got %q", key, raw))
	}
	return f
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

// inviteCodes parses "id:code,id:code". Never echoes a code into an error message.
func (p *parser) inviteCodes(key string) map[string]string {
	out := map[string]string{}
	for i, pair := range p.list(key, "") {
		id, code, ok := strings.Cut(pair, ":")
		id, code = strings.TrimSpace(id), strings.TrimSpace(code)
		if !ok || id == "" || code == "" {
			p.errs = append(p.errs, fmt.Errorf("%s entry %d must be id:code", key, i+1))
			continue
		}
		if strings.Contains(id, "|") {
			p.errs = append(p.errs, fmt.Errorf("%s id %q must not contain |", key, id)) // the cookie's separator
			continue
		}
		if _, dup := out[id]; dup {
			p.errs = append(p.errs, fmt.Errorf("%s has duplicate id %q", key, id))
		}
		out[id] = code
	}
	return out
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
