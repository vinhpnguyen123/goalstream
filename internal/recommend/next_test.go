package recommend

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	if got := cosine(vec(1, 0), vec(0, 1)); got != 0 {
		t.Fatalf("orthogonal: %v", got)
	}
	if got := cosine(vec(2, 0), vec(1, 0)); math.Abs(got-1) > 1e-12 {
		t.Fatalf("parallel: %v", got)
	}
	if got := cosine(vec(1, 0), vec()); got != 0 || math.IsNaN(got) { // a centroid that cancelled out
		t.Fatalf("zero vector: %v, want 0 (never NaN)", got)
	}
}

func TestRerank(t *testing.T) {
	cands := []candidate{{vec: vec(1, 0, 0)}, {vec: vec(0.9, 0.1, 0)}, {vec: vec(0.7, -0.7, 0)}}
	pos, neg := vec(1, 0, 0), vec(1, 1, 0)

	if got := rerank(cands, pos, nil, 0.5); got != 0 {
		t.Fatalf("neg nil: picked %d, want the nearest to pos (0)", got)
	}
	// 0 scores 1−0.5·0.71=0.65; 2 scores 0.71−0.5·0=0.71: the penalty changes the winner.
	if got := rerank(cands, pos, neg, 0.5); got != 2 {
		t.Fatalf("with penalty: picked %d, want 2", got)
	}
	if got := rerank(cands, nil, neg, 0.5); got != 2 {
		t.Fatalf("pos nil: picked %d, want the farthest from neg (2)", got)
	}
	if got := rerank([]candidate{{vec: vec(1)}, {vec: vec(1)}}, pos, nil, 0.5); got != 0 {
		t.Fatalf("tie: picked %d, want stage 1's first", got)
	}
}
