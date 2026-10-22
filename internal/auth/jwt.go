package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const issuer = "goalstream"

var ErrInvalidToken = errors.New("invalid access token")

// Signer issues and verifies access tokens: HS256, sub = user id, 15 minutes by default.
type Signer struct {
	secret []byte
	ttl    time.Duration
}

func NewSigner(secret string, ttl time.Duration) *Signer {
	return &Signer{secret: []byte(secret), ttl: ttl}
}

// Issue signs a token for userID valid from now for the signer's TTL. expiresIn is in seconds,
// the shape POST /auth/login returns.
func (s *Signer) Issue(userID string, now time.Time) (token string, expiresIn int, err error) {
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   userID,
		Issuer:    issuer,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
	})
	token, err = t.SignedString(s.secret)
	if err != nil {
		return "", 0, fmt.Errorf("signing access token: %w", err)
	}
	return token, int(s.ttl / time.Second), nil
}

// Verify returns the token's user id. The algorithm is pinned to HS256 — a token that names
// any other alg (none, HS512, RS256 …) is rejected before its signature is even checked.
// exp and iss are required; there is no leeway (one process signs and verifies). Every
// failure is the same error: the caller learns nothing about which check failed.
func (s *Signer) Verify(token string) (string, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil || uuid.Validate(claims.Subject) != nil {
		return "", ErrInvalidToken
	}
	return claims.Subject, nil
}
