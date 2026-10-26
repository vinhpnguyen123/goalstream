package transcode

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// §12 #22: the worked examples of IMPLEMENTATION_PLAN §10, exactly.
func TestLadderExamples(t *testing.T) {
	cases := []struct {
		name      string
		bps, h    int
		want      string // height@videoBps per rung, tallest first
		bandwidth []int  // checked where §10 states them
	}{
		{"A 720p low", 1_372_000, 720, "720@1372000 480@686000 360@343000", []int{1_675_842, 871_521, 486_160}},
		{"B 1080p capped", 5_072_000, 1080, "1080@5000000 720@2500000 480@1250000 360@625000", nil},
		{"C 1080p, last rung under minimum", 1_972_000, 1080, "1080@1972000 720@986000 480@493000", nil},
		{"D 576p starts at 480p", 1_404_000, 576, "480@1400000 360@700000", nil},
		{"E CRF mezzanine", 6_500_000, 720, "720@3000000 480@1400000 360@700000", nil},
		{"F below lowest height", 9_000_000, 240, "", nil},
		{"G below minimum at top", 250_000, 720, "", nil},
		{"1079p starts at 720p", 4_000_000, 1079, "720@3000000 480@1400000 360@700000", nil},
		{"1088p (padded) starts at 1080p", 4_000_000, 1088, "1080@4000000 720@2000000 480@1000000 360@500000", nil},
		{"exactly the minimum survives", 300_000, 360, "360@300000", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rungs, err := Ladder(tc.bps, tc.h)
			if tc.want == "" {
				if !errors.Is(err, ErrEmptyLadder) || rungs != nil {
					t.Fatalf("Ladder = %v, %v; want ErrEmptyLadder", rungs, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range rungs {
				got = append(got, fmt.Sprintf("%d@%d", r.Height, r.VideoBps))
			}
			if g := strings.Join(got, " "); g != tc.want {
				t.Fatalf("rungs = %s, want %s", g, tc.want)
			}
			for i, r := range rungs {
				if r.VideoBps > tc.bps {
					t.Errorf("%dp target %d above the source %d", r.Height, r.VideoBps, tc.bps)
				}
				if r.MaxRate != r.VideoBps*107/100 || r.BufSize != 2*r.VideoBps {
					t.Errorf("%dp maxrate/bufsize = %d/%d", r.Height, r.MaxRate, r.BufSize)
				}
				if want := map[bool]int{true: 128_000, false: 96_000}[r.Height >= 720]; r.AudioBps != want {
					t.Errorf("%dp audio = %d, want %d", r.Height, r.AudioBps, want)
				}
				if r.Bandwidth != (r.MaxRate+r.AudioBps)*105/100 {
					t.Errorf("%dp bandwidth %d is not the peak", r.Height, r.Bandwidth)
				}
				if tc.bandwidth != nil && r.Bandwidth != tc.bandwidth[i] {
					t.Errorf("%dp bandwidth = %d, want %d", r.Height, r.Bandwidth, tc.bandwidth[i])
				}
			}
		})
	}
}
