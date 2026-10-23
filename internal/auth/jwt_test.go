package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testJWTSecret = "0123456789abcdef0123456789abcdef"

func TestSignerRoundTrip(t *testing.T) {
	s := NewSigner(testJWTSecret, 15*time.Minute)
	user := uuid.NewString()
	tok, expiresIn, err := s.Issue(user, time.Now())
	if err != nil || expiresIn != 900 {
		t.Fatalf("Issue: %d, %v", expiresIn, err)
	}
	if got, err := s.Verify(tok); err != nil || got != user {
		t.Fatalf("Verify = %q, %v; want %q", got, err, user)
	}
}

// Only a token this signer issued, unexpired, passes. Everything else is ErrInvalidToken.
func TestSignerRejects(t *testing.T) {
	s := NewSigner(testJWTSecret, 15*time.Minute)
	user := uuid.NewString()
	now := time.Now()
	valid := func(c *jwt.RegisteredClaims) {
		c.Subject, c.Issuer = user, issuer
		c.IssuedAt, c.ExpiresAt = jwt.NewNumericDate(now), jwt.NewNumericDate(now.Add(time.Minute))
	}
	sign := func(m jwt.SigningMethod, key any, edit func(*jwt.RegisteredClaims)) string {
		var c jwt.RegisteredClaims
		valid(&c)
		edit(&c)
		tok, err := jwt.NewWithClaims(m, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	hs := func(edit func(*jwt.RegisteredClaims)) string {
		return sign(jwt.SigningMethodHS256, []byte(testJWTSecret), edit)
	}
	expired, _, _ := s.Issue(user, now.Add(-time.Hour))
	// Tampered: a valid token's header and signature around another user's payload.
	good := strings.Split(hs(func(*jwt.RegisteredClaims) {}), ".")
	forged := strings.Split(hs(func(c *jwt.RegisteredClaims) { c.Subject = uuid.NewString() }), ".")
	tampered := good[0] + "." + forged[1] + "." + good[2]

	for name, tok := range map[string]string{
		"expired":        expired,
		"other secret":   sign(jwt.SigningMethodHS256, []byte(strings.Repeat("x", 32)), func(*jwt.RegisteredClaims) {}),
		"wrong issuer":   hs(func(c *jwt.RegisteredClaims) { c.Issuer = "someone-else" }),
		"no issuer":      hs(func(c *jwt.RegisteredClaims) { c.Issuer = "" }),
		"no exp":         hs(func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil }),
		"sub not uuid":   hs(func(c *jwt.RegisteredClaims) { c.Subject = "admin" }),
		"no sub":         hs(func(c *jwt.RegisteredClaims) { c.Subject = "" }),
		"tampered claim": tampered,
		"alg none":       sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, func(*jwt.RegisteredClaims) {}),
		// Signed with the RIGHT secret: only the algorithm pin rejects it.
		"HS512":   sign(jwt.SigningMethodHS512, []byte(testJWTSecret), func(*jwt.RegisteredClaims) {}),
		"garbage": "not.a.token",
		"empty":   "",
	} {
		if got, err := s.Verify(tok); err != ErrInvalidToken || got != "" {
			t.Errorf("%s: Verify = %q, %v; want ErrInvalidToken", name, got, err)
		}
	}
}
