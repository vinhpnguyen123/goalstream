// Package recommend builds clip vectors and (from M4) selects clips. The vector layout is a
// versioned contract: docs/SCHEMA.md § Embedding v1. Changing an offset means `make reembed`.
package recommend

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const Dims = 128

// v1 layout (docs/SCHEMA.md).
const (
	teamOff, teamSlots     = 0, 64
	compOff, compSlots     = 64, 8
	seasonOff, seasonSlots = 72, 8
	tagOff, tagSlots       = 80, 32
	minuteDim              = 112
	homeDim                = 113
	awayDim                = 114
	minuteScale            = 120.0 // not 90: injury time must stay inside [0,1]
)

type Features struct {
	TeamSlot    *int // scoring team; nil when the annotation says "not applicable"
	CompSlot    int
	SeasonSlot  int
	TagSlots    []int
	MatchMinute int
	IsHome      *bool // nil → both home and away dims stay 0
}

// Embed builds the L2-normalized v1 vector. Slots are checked against the layout so a vocab
// bug can never write into a neighbouring feature block.
func Embed(f Features) ([]float32, error) {
	v := make([]float32, Dims)
	set := func(name string, off, n, slot int) error {
		if slot < 0 || slot >= n {
			return fmt.Errorf("embed: %s slot %d outside [0,%d)", name, slot, n)
		}
		v[off+slot] = 1
		return nil
	}
	if f.TeamSlot != nil {
		if err := set("team", teamOff, teamSlots, *f.TeamSlot); err != nil {
			return nil, err
		}
	}
	if err := set("competition", compOff, compSlots, f.CompSlot); err != nil {
		return nil, err
	}
	if err := set("season", seasonOff, seasonSlots, f.SeasonSlot); err != nil {
		return nil, err
	}
	for _, s := range f.TagSlots {
		if err := set("tag", tagOff, tagSlots, s); err != nil {
			return nil, err
		}
	}
	v[minuteDim] = float32(math.Min(math.Max(float64(f.MatchMinute)/minuteScale, 0), 1))
	if f.IsHome != nil {
		if *f.IsHome {
			v[homeDim] = 1
		} else {
			v[awayDim] = 1
		}
	}

	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum)) // never 0: competition and season are always set
	for i := range v {
		v[i] /= norm
	}
	return v, nil
}

// Literal renders v in pgvector's text form, "[a,b,…]", for a `$n::vector` parameter.
func Literal(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
