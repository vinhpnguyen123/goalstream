package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const secret = "0123456789abcdef0123456789abcdef"

func TestVerifyInvite(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	codes := map[string]string{"me": "let-me-in"}
	good := SignInvite(secret, "me", now.Add(time.Hour))

	forged := func() string { // valid shape, MAC computed with the wrong secret
		return SignInvite("another-secret-of-at-least-32-bytes", "me", now.Add(time.Hour))
	}
	tamperedExp := func() string { // push exp forward, keep the old MAC
		raw, _ := base64.RawURLEncoding.DecodeString(good)
		p := strings.Split(string(raw), "|")
		return base64.RawURLEncoding.EncodeToString([]byte(p[0] + "|9999999999|" + p[2]))
	}

	cases := []struct {
		name   string
		cookie string
		codes  map[string]string
		now    time.Time
		ok     bool
	}{
		{"valid", good, codes, now, true},
		{"bad hmac", forged(), codes, now, false},
		{"tampered exp", tamperedExp(), codes, now, false},
		{"expired", good, codes, now.Add(2 * time.Hour), false},
		{"exactly at exp", good, codes, now.Add(time.Hour), false},
		{"revoked id", good, map[string]string{"friend": "x"}, now, false},
		{"not base64", "%%%", codes, now, false},
		{"wrong shape", base64.RawURLEncoding.EncodeToString([]byte("me|1")), codes, now, false},
		{"empty", "", codes, now, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := VerifyInvite(secret, tc.cookie, tc.codes, tc.now)
			if tc.ok && (err != nil || id != "me") {
				t.Fatalf("want valid, got id=%q err=%v", id, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("want rejection, got id=%q", id)
			}
		})
	}
}

func TestMatchInviteCode(t *testing.T) {
	codes := map[string]string{"me": "let-me-in", "friend": "open-sesame"}
	for _, tc := range []struct {
		code, want string
		ok         bool
	}{
		{"let-me-in", "me", true},
		{"open-sesame", "friend", true},
		{"let-me-i", "", false},
		{"let-me-in ", "", false},
		{"", "", false},
	} {
		if id, ok := MatchInviteCode(codes, tc.code); id != tc.want || ok != tc.ok {
			t.Errorf("code %q: got %q,%v want %q,%v", tc.code, id, ok, tc.want, tc.ok)
		}
	}
}
