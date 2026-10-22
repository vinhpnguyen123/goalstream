package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/audit"
)

var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid credentials")
)

const maxDisplayName = 64

type User struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	DisplayName *string `json:"display_name"`
}

// NormalizeEmail trims and lowercases (users_email_lower_chk stores exactly that) and checks
// the shape docs/API.md asks for: contains '@', at most 254 bytes.
func NormalizeEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	return e, strings.Contains(e, "@") && len(e) <= 254
}

// ValidPassword: 10–128 characters, counted as runes so "é" is one character, not two bytes.
func ValidPassword(pw string) bool {
	n := utf8.RuneCountInString(pw)
	return n >= 10 && n <= 128
}

// NormalizeDisplayName trims; empty is NULL. More than maxDisplayName characters is invalid.
func NormalizeDisplayName(raw *string) (*string, bool) {
	if raw == nil {
		return nil, true
	}
	n := strings.TrimSpace(*raw)
	if n == "" {
		return nil, true
	}
	return &n, utf8.RuneCountInString(n) <= maxDisplayName
}

// Register creates a user and audits user_registered in one transaction. It does not log in.
// Inputs must already be normalized and validated.
func Register(ctx context.Context, pool *pgxpool.Pool, email, password string, displayName *string) (User, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	u := User{Email: email, DisplayName: displayName}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, display_name) VALUES ($1, $2, $3) RETURNING id`,
			email, hash, displayName).Scan(&u.ID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_key" {
			return ErrEmailTaken
		} else if err != nil {
			return fmt.Errorf("inserting user: %w", err)
		}
		return audit.Write(ctx, tx, audit.Entry{
			EntityType: "user", EntityID: u.ID, Action: "user_registered", ActorType: "user",
			Metadata: map[string]any{},
		})
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// checkPassword is CheckPassword; a variable only so a test can count the comparisons.
var checkPassword = CheckPassword

// dummyHash is compared against when there is no real hash to check, so an unknown email
// costs the same argon2id work as a wrong password.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("goalstream-dummy-password")
	if err != nil {
		panic(err) // crypto/rand failing: nothing about the process can be trusted
	}
	return h
})

// Login returns the user id for email + password. It always runs exactly ONE argon2id
// comparison — against dummyHash when the email is unknown or the account cannot log in —
// so the response time does not reveal which emails exist. Every failure is
// ErrInvalidCredentials. Login failures are not audited (plan §6): the limiter covers them.
func Login(ctx context.Context, pool *pgxpool.Pool, email, password string) (string, error) {
	var id, hash string
	err := pool.QueryRow(ctx, `SELECT id, password_hash FROM users WHERE email = $1`, email).Scan(&id, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("reading user: %w", err)
	}
	known := err == nil && hash != NoPassword
	if !known {
		hash = dummyHash()
	}
	ok, err := checkPassword(password, hash)
	if err != nil {
		return "", err
	}
	if !known || !ok {
		return "", ErrInvalidCredentials
	}
	return id, nil
}
