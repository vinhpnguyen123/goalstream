package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/audit"
)

const RefreshCookie = "gs_refresh"

// ErrRefreshInvalid is every refused refresh — unknown, expired, revoked, reused. The caller
// answers 401 and clears the cookie; it learns nothing about which case it was.
var ErrRefreshInvalid = errors.New("refresh token rejected")

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

// Rotate exchanges a refresh token for its successor in the same family (plan §13 M4,
// docs/API.md § POST /auth/refresh). One transaction:
//
//   - unknown or expired → ErrRefreshInvalid; the family is left alone (an expired token is
//     worthless, and a pruned one is simply unknown).
//   - active → the guarded rotation; its successor gets a fresh ttl (sliding expiry).
//   - rotated less than grace ago, in a family nobody revoked → another successor. Two tabs, or
//     React StrictMode's double effect, refresh with one cookie milliseconds apart; without
//     the grace that race is indistinguishable from theft and logs the user out.
//   - anything else (rotated longer ago, or revoked for reuse/logout) → someone replayed a
//     token that was already replaced. Thief or victim cannot be told apart, so the WHOLE
//     family is revoked ('reuse') and audited; both must log in again.
func Rotate(ctx context.Context, pool *pgxpool.Pool, token string, ttl, grace time.Duration) (userID, next string, err error) {
	var reuse *string // family to revoke, decided inside the tx and committed with it
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var id, family string
		var expired, withinGrace bool
		var reason *string
		// FOR UPDATE: two concurrent refreshes of one token queue here instead of both reading
		// "active". The second then sees the first's rotation and takes the grace branch.
		err := tx.QueryRow(ctx, `
			SELECT id, user_id, family_id, expires_at <= now(),
			       COALESCE(revoked_at > now() - make_interval(secs => $2), false), revoked_reason
			  FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`, HashRefresh(token), grace.Seconds()).
			Scan(&id, &userID, &family, &expired, &withinGrace, &reason)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRefreshInvalid
		} else if err != nil {
			return fmt.Errorf("reading refresh token: %w", err)
		}
		if expired {
			return ErrRefreshInvalid
		}

		if reason == nil {
			tag, err := tx.Exec(ctx, `
				UPDATE refresh_tokens SET revoked_at = now(), revoked_reason = 'rotated'
				 WHERE id = $1 AND revoked_at IS NULL`, id)
			if err != nil {
				return fmt.Errorf("rotating refresh token: %w", err)
			}
			if tag.RowsAffected() == 1 {
				next, err = IssueRefresh(ctx, tx, userID, family, ttl)
				return err
			}
			// 0 rows: rotated since we read it. The row lock makes this unreachable today, but
			// the guard is what the plan specifies; treat it as the race it would be.
			r := "rotated"
			reason, withinGrace = &r, true
		}

		if *reason == "rotated" && withinGrace {
			var dead bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM refresh_tokens
				                WHERE family_id = $1 AND revoked_reason IN ('reuse','logout'))`, family).Scan(&dead); err != nil {
				return fmt.Errorf("reading refresh family: %w", err)
			}
			if dead {
				// Logged out (or already caught) inside the grace: no new token in a dead family.
				return ErrRefreshInvalid
			}
			next, err = IssueRefresh(ctx, tx, userID, family, ttl)
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE refresh_tokens SET revoked_at = now(), revoked_reason = 'reuse'
			 WHERE family_id = $1 AND revoked_at IS NULL`, family); err != nil {
			return fmt.Errorf("revoking refresh family: %w", err)
		}
		if err := audit.Write(ctx, tx, audit.Entry{
			EntityType: "user", EntityID: userID, Action: "refresh_reuse_detected", ActorType: "system",
			Metadata: map[string]any{"family_id": family},
		}); err != nil {
			return err
		}
		reuse = &family
		return nil // commit the revocation; the caller still gets ErrRefreshInvalid
	})
	if err != nil {
		return "", "", err
	}
	if reuse != nil {
		return "", "", ErrRefreshInvalid
	}
	return userID, next, nil
}

// Logout revokes the token's whole family ('logout'): every device that shares this login is
// signed out. An unknown token is not an error — logout is idempotent.
func Logout(ctx context.Context, pool *pgxpool.Pool, token string) error {
	if _, err := pool.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now(), revoked_reason = 'logout'
		 WHERE revoked_at IS NULL
		   AND family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`, HashRefresh(token)); err != nil {
		return fmt.Errorf("revoking refresh family: %w", err)
	}
	return nil
}

// PruneRefresh deletes tokens expired for more than a day. A pruned token that is replayed is
// unknown → 401 without a family revoke. Fine: it had expired anyway.
func PruneRefresh(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < now() - interval '1 day'`)
	if err != nil {
		return 0, fmt.Errorf("pruning refresh tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}
