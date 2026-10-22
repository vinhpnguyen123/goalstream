package auth

import (
	"fmt"

	"github.com/alexedwards/argon2id"
)

// NoPassword is the password_hash of an account that can never log in (the dev user).
const NoPassword = "!"

// HashPassword returns the argon2id encoded hash (library default parameters, plan §13 M4).
// The salt is random, so the same password never hashes to the same string twice.
func HashPassword(pw string) (string, error) {
	h, err := argon2id.CreateHash(pw, argon2id.DefaultParams)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return h, nil
}

// CheckPassword reports whether pw matches hash, in constant time. NoPassword matches
// nothing. Any other unparseable hash is an error: a corrupt row, not a wrong password.
func CheckPassword(pw, hash string) (bool, error) {
	if hash == NoPassword {
		return false, nil
	}
	ok, err := argon2id.ComparePasswordAndHash(pw, hash)
	if err != nil {
		return false, fmt.Errorf("checking password: %w", err)
	}
	return ok, nil
}
