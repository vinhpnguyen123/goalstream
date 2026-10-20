package recommend

import (
	"math"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func nonZero(v []float32) map[int]bool {
	m := map[int]bool{}
	for i, x := range v {
		if x != 0 {
			m[i] = true
		}
	}
	return m
}

func TestEmbedLayout(t *testing.T) {
	v, err := Embed(Features{
		TeamSlot: ptr(3), CompSlot: 1, SeasonSlot: 0, TagSlots: []int{0, 12},
		MatchMinute: 60, IsHome: ptr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != Dims {
		t.Fatalf("len %d", len(v))
	}
	want := map[int]bool{3: true, 65: true, 72: true, 80: true, 92: true, 112: true, 113: true}
	got := nonZero(v)
	if len(got) != len(want) {
		t.Fatalf("non-zero dims %v, want %v", got, want)
	}
	for d := range want {
		if !got[d] {
			t.Fatalf("dim %d is zero; non-zero dims %v", d, got)
		}
	}
	if math.Abs(norm(v)-1) > 1e-6 {
		t.Fatalf("norm %f, want 1", norm(v))
	}
	// minute 60/120 = 0.5 relative to a one-hot 1.0, before normalization
	if r := v[112] / v[3]; math.Abs(float64(r)-0.5) > 1e-6 {
		t.Fatalf("minute ratio %f, want 0.5", r)
	}
}

func TestEmbedEdges(t *testing.T) {
	// Not-applicable team and is_home: no team dim, neither home nor away.
	v, err := Embed(Features{CompSlot: 0, SeasonSlot: 0, MatchMinute: 10})
	if err != nil {
		t.Fatal(err)
	}
	for d := range nonZero(v) {
		if d < 64 || d == 113 || d == 114 {
			t.Fatalf("dim %d set for a NULL team / NULL is_home", d)
		}
	}
	// Away goal sets 114, not 113.
	v, _ = Embed(Features{CompSlot: 0, SeasonSlot: 0, IsHome: ptr(false)})
	if v[113] != 0 || v[114] == 0 {
		t.Fatal("away goal must set dim 114 only")
	}
	// Injury time beyond 120 clamps to 1 (relative), negative clamps to 0.
	big, _ := Embed(Features{CompSlot: 0, SeasonSlot: 0, MatchMinute: 130})
	if r := big[112] / big[64]; math.Abs(float64(r)-1) > 1e-6 {
		t.Fatalf("minute 130 not clamped: ratio %f", r)
	}
	// Out-of-range slots are errors, never writes into a neighbouring block.
	for name, f := range map[string]Features{
		"team 64":       {TeamSlot: ptr(64)},
		"competition 8": {CompSlot: 8},
		"season -1":     {SeasonSlot: -1},
		"tag 32":        {TagSlots: []int{32}},
	} {
		if _, err := Embed(f); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestLiteral(t *testing.T) {
	if got := Literal([]float32{1, 0, 0.5}); got != "[1,0,0.5]" {
		t.Fatalf("got %q", got)
	}
}
