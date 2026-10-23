package recommend

import (
	"math"
	"strings"
	"testing"
)

func vec(xs ...float32) []float32 {
	v := make([]float32, Dims)
	copy(v, xs)
	return v
}

func near(t *testing.T, got, want []float32) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("got %v, want %v", got == nil, want == nil)
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Fatalf("dim %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestAddRemoveVec(t *testing.T) {
	a, b, c := vec(1, 0, 3), vec(0, 2, 0), vec(2, 4, -3)

	first := addVec(nil, 0, a)
	near(t, first, a)
	first[0] = 99 // a copy, not an alias of the clip's vector
	if a[0] != 1 {
		t.Fatal("addVec(nil, 0, v) aliases v")
	}

	ab := addVec(a, 1, b)
	near(t, ab, vec(0.5, 1, 1.5))
	abc := addVec(ab, 2, c)
	near(t, abc, vec(1, 2, 0))

	near(t, removeVec(abc, 3, c), ab) // remove undoes add
	near(t, removeVec(ab, 2, a), b)   // in any order
	if removeVec(a, 1, a) != nil {
		t.Fatal("removing the only vector must leave nil (taste_*_chk)")
	}
}

func TestParseVector(t *testing.T) {
	v := vec(0.1, -2.5e-7, 3, 1.0/3)
	got, err := ParseVector(Literal(v))
	if err != nil {
		t.Fatal(err)
	}
	for i := range v {
		if got[i] != v[i] { // exact: Literal prints the shortest float32 round-trip form
			t.Fatalf("dim %d: got %v, want %v", i, got[i], v[i])
		}
	}

	for name, s := range map[string]string{
		"empty":      "",
		"no bracket": strings.Trim(Literal(v), "[]"),
		"short":      "[1,2,3]",
		"not number": "[x" + Literal(v)[2:],
	} {
		if _, err := ParseVector(s); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// The transitions of plan §11, from a taste holding one 'loved' (a) and one 'not_for_me' (b).
func TestTasteMove(t *testing.T) {
	a, b, c := vec(1), vec(0, 1), vec(0, 0, 1)
	for _, tc := range []struct {
		prev, next  string
		wantPos     []float32
		wantNeg     []float32
		liked, disl int
	}{
		{"", "loved", vec(0.5, 0, 0.5), vec(0, 1), 2, 1},     // none → loved: add
		{"", "okay", a, vec(0, 1), 1, 1},                     // none → okay: nothing
		{"loved", "not_for_me", nil, vec(0, 0.5, 0.5), 0, 2}, // pos → neg; pos had only c
		{"loved", "okay", nil, vec(0, 1), 0, 1},              // remove only
		{"okay", "loved", vec(0.5, 0, 0.5), vec(0, 1), 2, 1}, // add only
		{"not_for_me", "loved", vec(0.5, 0, 0.5), nil, 2, 0}, // neg → pos; neg had only c
	} {
		// The taste already holds c under prev, as it would after rating c that way.
		tt := taste{pos: a, neg: b, liked: 1, disliked: 1}
		switch tc.prev {
		case "loved":
			tt = taste{pos: c, neg: b, liked: 1, disliked: 1}
		case "not_for_me":
			tt = taste{pos: a, neg: c, liked: 1, disliked: 1}
		}
		tt.move(tc.prev, tc.next, c)
		if tt.liked != tc.liked || tt.disliked != tc.disl {
			t.Fatalf("%s→%s: counts %d/%d, want %d/%d", tc.prev, tc.next, tt.liked, tt.disliked, tc.liked, tc.disl)
		}
		near(t, tt.pos, tc.wantPos)
		near(t, tt.neg, tc.wantNeg)
	}
}
