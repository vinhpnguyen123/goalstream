package transcode

import "errors"

// Rung is one HLS rendition. Bitrates are bits/sec.
type Rung struct {
	Height    int
	VideoBps  int // target (average): -b:v, never above the source
	MaxRate   int // -maxrate: VideoBps + 7% VBV headroom
	BufSize   int // -bufsize: 2 × VideoBps
	AudioBps  int
	Bandwidth int // PEAK video + audio + 5% container: master.m3u8 BANDWIDTH
}

// ErrEmptyLadder means no rung fits the source: below the lowest height, or below minRung at
// the top. A deterministic, terminal failure (bad_ladder).
var ErrEmptyLadder = errors.New("no rung fits the source")

// The ladder's only numbers (IMPLEMENTATION_PLAN §10, D3). ceilings is ordered tallest first:
// each height caps what it is worth spending, whatever the source offers.
var ceilings = []struct{ height, maxBps int }{
	{1080, 5_000_000},
	{720, 3_000_000},
	{480, 1_400_000},
	{360, 700_000},
}

const minRung = 300_000

func audioFor(height int) int {
	if height >= 720 {
		return 128_000
	}
	return 96_000
}

// Ladder derives the renditions for a source from its probed VIDEO STREAM bitrate and height.
//
// Never above the source (principle #4): a rung is skipped when taller than the source (no
// upscaling) and its bitrate starts at the source's. Each step also caps at the height's
// ceiling — our source is a CRF mezzanine whose bitrate alone is an inflated cap — and then
// halves (~2× spacing: closer rungs flap, wider ones make each switch jarring). Halving below
// minRung ends the ladder.
func Ladder(srcVideoBps, srcHeight int) ([]Rung, error) {
	var rungs []Rung
	b := srcVideoBps
	for _, c := range ceilings {
		if c.height > srcHeight {
			continue // strict: a 1079p source starts at 720p, a 1088p (padded) one at 1080p
		}
		b = min(b, c.maxBps)
		if b < minRung {
			break
		}
		a := audioFor(c.height)
		peak := b * 107 / 100
		rungs = append(rungs, Rung{
			Height: c.height, VideoBps: b, MaxRate: peak, BufSize: 2 * b, AudioBps: a,
			Bandwidth: (peak + a) * 105 / 100,
		})
		b /= 2
	}
	if len(rungs) == 0 {
		return nil, ErrEmptyLadder
	}
	return rungs, nil
}
