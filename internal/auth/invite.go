// Package auth holds credentials: the invite cookie (M1), JWT and refresh rotation (M4).
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

const InviteCookie = "gs_invite"

var ErrInvalidInvite = errors.New("invalid invite cookie")

// SignInvite builds the gs_invite value: base64url(id|exp|HMAC-SHA256(secret, id|exp))
// (docs/API.md § Cookies). Configured ids never contain "|" (config rejects them).
func SignInvite(secret, id string, exp time.Time) string {
	payload := id + "|" + strconv.FormatInt(exp.Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + mac(secret, payload)))
}

// VerifyInvite returns the invite id when the MAC verifies (constant time), exp is in the
// future, and the id is still configured — removing an id revokes every cookie it issued.
// One error for every failure: the caller learns nothing about which check failed.
func VerifyInvite(secret, cookie string, codes map[string]string, now time.Time) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil {
		return "", ErrInvalidInvite
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return "", ErrInvalidInvite
	}
	id, expS, sig := parts[0], parts[1], parts[2]
	if !hmac.Equal([]byte(sig), []byte(mac(secret, id+"|"+expS))) {
		return "", ErrInvalidInvite
	}
	exp, err := strconv.ParseInt(expS, 10, 64)
	if err != nil || !now.Before(time.Unix(exp, 0)) {
		return "", ErrInvalidInvite
	}
	if _, ok := codes[id]; !ok {
		return "", ErrInvalidInvite
	}
	return id, nil
}

// MatchInviteCode returns the id whose code equals code. Every configured code is compared,
// each in constant time over SHA-256 digests (equal length, so the length of a real code
// does not leak either), and the loop never exits early.
func MatchInviteCode(codes map[string]string, code string) (string, bool) {
	got := sha256.Sum256([]byte(code))
	match := ""
	for id, c := range codes {
		want := sha256.Sum256([]byte(c))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			match = id
		}
	}
	return match, match != ""
}

func mac(secret, payload string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}
