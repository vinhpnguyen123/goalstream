package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/audit"
)

const RefreshCookie = "gs_refresh"

// HashRefresh is what refresh_tokens.token_hash stores: SHA-256 hex. The token itself is
// never stored or logged. 32 random bytes need no slow hash — there is nothing to brute-force.
func HashRefresh(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueRefresh mints a refresh token in familyID (a new UUID at login; the old token's family
// on rotation, M4 S3) and stores its hash. It returns the cookie value.
func IssueRefresh(ctx context.Context, q audit.Execer, userID, familyID string, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating refresh token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	if _, err := q.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, family_id, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))`,
		userID, HashRefresh(token), familyID, ttl.Seconds()); err != nil {
		return "", fmt.Errorf("storing refresh token: %w", err)
	}
	return token, nil
}
