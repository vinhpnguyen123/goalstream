package recommend

import (
	"fmt"
	"strconv"
	"strings"
)

// A centroid is the running mean of n clip vectors (plan §11). nil ⇔ n == 0, which is exactly
// user_taste's taste_pos_chk / taste_neg_chk. The arithmetic runs in float64 and rounds to
// float32 once per step, to keep the drift of repeated subtract-then-add small; a rebuild
// (make reembed) resets it.

// addVec returns the mean of the n vectors behind c plus v: (c·n + v) / (n+1).
func addVec(c []float32, n int, v []float32) []float32 {
	out := make([]float32, len(v))
	if n == 0 {
		copy(out, v)
		return out
	}
	for i := range v {
		out[i] = float32((float64(c[i])*float64(n) + float64(v[i])) / float64(n+1))
	}
	return out
}

// removeVec returns the mean of the n vectors behind c minus v: (c·n − v) / (n−1). Removing
// the only vector leaves no mean at all — nil, not a zero vector. v must be a vector that was
// added: the rebuild lock guarantees it (see LockRebuild).
func removeVec(c []float32, n int, v []float32) []float32 {
	if n <= 1 {
		return nil
	}
	out := make([]float32, len(v))
	for i := range v {
		out[i] = float32((float64(c[i])*float64(n) - float64(v[i])) / float64(n-1))
	}
	return out
}

// ParseVector reads pgvector's text form, the inverse of Literal.
func ParseVector(s string) ([]float32, error) {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("vector %.20q: not [..]", s)
	}
	parts := strings.Split(s[1:len(s)-1], ",")
	if len(parts) != Dims {
		return nil, fmt.Errorf("vector has %d dims, want %d", len(parts), Dims)
	}
	v := make([]float32, Dims)
	for i, p := range parts {
		x, err := strconv.ParseFloat(p, 32)
		if err != nil {
			return nil, fmt.Errorf("vector dim %d: %w", i, err)
		}
		v[i] = float32(x)
	}
	return v, nil
}

// literalOrNil is Literal for a centroid column: nil stays SQL NULL.
func literalOrNil(v []float32) *string {
	if v == nil {
		return nil
	}
	s := Literal(v)
	return &s
}
