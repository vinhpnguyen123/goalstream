package ingest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/transcode"
)

// A real Labels-v2.json excerpt (DATASET.md): position is a string.
const realLabels = `{
  "UrlLocal": "england_epl/2014-2015/2015-02-21 - 18-00 Swansea 2 - 1 Manchester United/",
  "UrlYoutube": "", "gameHomeTeam": "Swansea", "gameAwayTeam": "Manchester United",
  "gameDate": "21/02/2015 - 18:00", "gameScore": "2 - 1",
  "annotations": [
    {"gameTime": "1 - 27:36", "label": "Goal", "position": "1656477", "team": "away", "visibility": "visible"}
  ]}`

func TestParseLabelsAndMeta(t *testing.T) {
	l, err := ParseLabels(strings.NewReader(realLabels))
	if err != nil {
		t.Fatal(err)
	}
	m, err := l.Meta()
	if err != nil {
		t.Fatal(err)
	}
	if m.Home != "Swansea" || m.Away != "Manchester United" || !m.Date.Equal(time.Date(2015, 2, 21, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("meta %+v", m)
	}
	if len(l.Annotations) != 1 || l.Annotations[0].Position != "1656477" {
		t.Fatalf("annotations %+v", l.Annotations)
	}
	if _, err := ParseLabels(strings.NewReader("{broken")); err == nil {
		t.Fatal("broken json must fail")
	}
}

func TestMetaRejects(t *testing.T) {
	cases := map[string]Labels{
		"no home":        {GameAwayTeam: "Y", GameDate: "30/08/2015 - 15:00"},
		"blank away":     {GameHomeTeam: "X", GameAwayTeam: "  ", GameDate: "30/08/2015 - 15:00"},
		"no date":        {GameHomeTeam: "X", GameAwayTeam: "Y"},
		"us date format": {GameHomeTeam: "X", GameAwayTeam: "Y", GameDate: "08/30/2015 - 15:00"},
	}
	for name, l := range cases {
		if _, err := l.Meta(); !errors.Is(err, errBadMeta) {
			t.Errorf("%s: want bad_match_metadata, got %v", name, err)
		}
	}
}

func TestParseEvent(t *testing.T) {
	cases := []struct {
		gameTime, position string
		half, pos          int
		bad                bool
	}{
		{"1 - 27:36", "1656477", 1, 1656477, false},
		{"2 - 48:33", "2913948", 2, 2913948, false},
		{" 2 - 00:00 ", " 0 ", 2, 0, false},
		{"3 - 00:10", "10000", 0, 0, true},      // no third half
		{"10 - 00:10", "10000", 0, 0, true},     // "1" prefix but not half 1
		{"1-27:36", "1656477", 0, 0, true},      // malformed separator
		{"", "1", 0, 0, true},                   // empty gameTime
		{"1 - 27:36", "27:36", 0, 0, true},      // position not an integer
		{"1 - 27:36", "-5", 0, 0, true},         // negative
		{"1 - 27:36", "9999999999", 0, 0, true}, // overflows clips.position_ms INT
	}
	for _, tc := range cases {
		half, pos, err := ParseEvent(Annotation{GameTime: tc.gameTime, Position: tc.position})
		if tc.bad {
			if !errors.Is(err, errBadAnnotation) {
				t.Errorf("%q/%q: want bad_annotation, got %v", tc.gameTime, tc.position, err)
			}
			continue
		}
		if err != nil || half != tc.half || pos != tc.pos {
			t.Errorf("%q/%q: got (%d,%d,%v), want (%d,%d)", tc.gameTime, tc.position, half, pos, err, tc.half, tc.pos)
		}
	}
}

func TestFolderMatches(t *testing.T) {
	cases := []struct {
		folder, home, away string
		want               bool
	}{
		{"2015-02-21 - 18-00 Swansea 2 - 1 Manchester United", "Swansea", "Manchester United", true},
		{"2015-08-22 - 15-00 Mainz 05 2 - 1 Hannover 96", "Mainz 05", "Hannover 96", true}, // digits in names
		{"2016-08-14 - 18-00 1899 Hoffenheim 1 - 0 Werder Bremen", "1899 Hoffenheim", "Werder Bremen", true},
		{"2017-03-08 - 22-45 Barcelona 6 - 1 Paris SG", "Barcelona", "Paris SG", true},
		{"2017-03-08 - 22-45 Barcelona 6 - 1 Paris SG", "Barcelona", "PSG", false}, // JSON disagrees
		{"not a match folder", "X", "Y", false},
		{"2015-08-30 - 15-00 X - Y", "X", "Y", false}, // no score
	}
	for _, tc := range cases {
		if got := FolderMatches(tc.folder, tc.home, tc.away); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.folder, got, tc.want)
		}
	}
}

func TestMatchMinuteAndTitle(t *testing.T) {
	cases := []struct {
		half, pos int
		minute    int
		title     string
	}{
		{1, 0, 1, "A vs B — Goal 1'"},
		{1, 862_000, 15, "A vs B — Goal 15'"},     // 14:22 is the 15th minute
		{1, 2_699_999, 45, "A vs B — Goal 45'"},   // 44:59.999
		{1, 2_708_042, 46, "A vs B — Goal 45+1'"}, // real: Palace–Arsenal 45:08
		{2, 0, 46, "A vs B — Goal 46'"},
		{2, 1_627_793, 73, "A vs B — Goal 73'"},
		{2, 2_699_999, 90, "A vs B — Goal 90'"},
		{2, 2_913_948, 94, "A vs B — Goal 90+4'"}, // real: Palace–Arsenal 48:33
	}
	for _, tc := range cases {
		m := MatchMinute(tc.half, tc.pos)
		if m != tc.minute {
			t.Errorf("MatchMinute(%d,%d)=%d want %d", tc.half, tc.pos, m, tc.minute)
		}
		if got := Title("A", "B", "Goal", tc.half, m); got != tc.title {
			t.Errorf("Title(h%d,%d)=%q want %q", tc.half, m, got, tc.title)
		}
	}
}

func TestCutWindow(t *testing.T) {
	const pre, post, half = 15_000, 45_000, 180_000 // the fixture halves are 180 s
	cases := []struct {
		name       string
		pos        int
		start, dur int
		outOfRange bool
	}{
		{"normal", 60_000, 45_000, 60_000, false},
		{"clamped at start", 5_000, 0, 50_000, false},
		{"exactly at pre-roll", 15_000, 0, 60_000, false},
		{"clamped at end", 170_000, 155_000, 25_000, false}, // QC then rejects as too_short
		{"last millisecond", 179_999, 164_999, 15_001, false},
		{"at end of file", 180_000, 0, 0, true},
		{"past end of file", 999_999, 0, 0, true},
	}
	for _, tc := range cases {
		w, err := CutWindow(tc.pos, half, pre, post)
		if tc.outOfRange {
			if !errors.Is(err, errOutOfRange) {
				t.Errorf("%s: want position_out_of_range, got %v", tc.name, err)
			}
			continue
		}
		if err != nil || w.StartMS != tc.start || w.DurationMS != tc.dur {
			t.Errorf("%s: got %+v %v, want start %d dur %d", tc.name, w, err, tc.start, tc.dur)
		}
	}
}

func TestQC(t *testing.T) {
	ok := transcode.Probe{HasVideo: true, HasAudio: true, DurationMS: 60_000}
	noAudio, short, noVideo := ok, ok, ok
	noAudio.HasAudio, short.DurationMS, noVideo.HasVideo = false, 25_000, false
	exactly := ok
	exactly.DurationMS = 30_000

	cases := []struct {
		name   string
		p      transcode.Probe
		decode error
		want   string
	}{
		{"pass", ok, nil, ""},
		{"exactly the minimum passes", exactly, nil, ""},
		{"no video", noVideo, nil, ReasonNoVideoStream},
		{"no audio", noAudio, nil, ReasonNoAudioStream},
		{"too short", short, nil, ReasonTooShort},
		{"decode failed", ok, errors.New("corrupt"), ReasonDecodeFailed},
	}
	for _, tc := range cases {
		if got := QC(tc.p, 30_000, tc.decode); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestMezzanineCap(t *testing.T) {
	cases := []struct{ src, ceiling, want int }{
		{2_824_198, 8_000_000, 2_824_198},  // real SoccerNet half: the source wins (D15)
		{12_000_000, 8_000_000, 8_000_000}, // a heavy source: the ceiling wins
		{0, 8_000_000, 8_000_000},          // bitrate not reported: ceiling alone
	}
	for _, tc := range cases {
		if got := MezzanineCap(tc.src, tc.ceiling); got != tc.want {
			t.Errorf("MezzanineCap(%d,%d)=%d want %d", tc.src, tc.ceiling, got, tc.want)
		}
	}
}
