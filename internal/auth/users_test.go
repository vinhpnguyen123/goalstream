package auth

import (
	"strings"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"me@x.com", "me@x.com", true},
		{"  Me@X.Com \t", "me@x.com", true},
		{"no-at-sign", "no-at-sign", false},
		{"", "", false},
		{strings.Repeat("a", 248) + "@x.com", strings.Repeat("a", 248) + "@x.com", true}, // 254 bytes
		{strings.Repeat("a", 249) + "@x.com", strings.Repeat("a", 249) + "@x.com", false},
	} {
		if got, ok := NormalizeEmail(c.in); got != c.want || ok != c.ok {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestValidPassword(t *testing.T) {
	for _, c := range []struct {
		pw string
		ok bool
	}{
		{strings.Repeat("a", 9), false},
		{strings.Repeat("a", 10), true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
		{strings.Repeat("é", 10), true},  // 20 bytes, 10 characters
		{strings.Repeat("é", 9), false},  // 18 bytes but only 9 characters
		{strings.Repeat("é", 128), true}, // 256 bytes, 128 characters
	} {
		if got := ValidPassword(c.pw); got != c.ok {
			t.Errorf("ValidPassword(%d runes, %d bytes) = %v, want %v", len([]rune(c.pw)), len(c.pw), got, c.ok)
		}
	}
}

func TestNormalizeDisplayName(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		name string
		in   *string
		want *string
		ok   bool
	}{
		{"absent", nil, nil, true},
		{"blank is NULL", s("   "), nil, true},
		{"trimmed", s("  Vinh  "), s("Vinh"), true},
		{"64 characters", s(strings.Repeat("ê", 64)), s(strings.Repeat("ê", 64)), true},
		{"65 characters", s(strings.Repeat("a", 65)), nil, false},
	} {
		got, ok := NormalizeDisplayName(c.in)
		if ok != c.ok || (c.ok && (got == nil) != (c.want == nil)) || (c.ok && got != nil && *got != *c.want) {
			t.Errorf("%s: got %v, %v", c.name, got, ok)
		}
	}
}
