// Package ingest turns SoccerNet matches (two half files + Labels-v2.json) into clips.
// Format facts are in docs/DATASET.md, which was verified against real downloads.
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Rejection reasons — a closed vocabulary (IMPLEMENTATION_PLAN §6). They are written to the
// audit log, so they never change spelling.
const (
	ReasonMissingLabels      = "missing_labels"
	ReasonBadMatchMetadata   = "bad_match_metadata"
	ReasonMissingVideo       = "missing_video"
	ReasonBadAnnotation      = "bad_annotation"
	ReasonPositionOutOfRange = "position_out_of_range"
	ReasonNotShown           = "not_shown"
	ReasonNoVideoStream      = "no_video_stream"
	ReasonNoAudioStream      = "no_audio_stream"
	ReasonTooShort           = "too_short"
	ReasonDecodeFailed       = "decode_failed"
	ReasonCutFailed          = "cut_failed"
)

type Labels struct {
	UrlLocal     string       `json:"UrlLocal"`
	GameHomeTeam string       `json:"gameHomeTeam"`
	GameAwayTeam string       `json:"gameAwayTeam"`
	GameDate     string       `json:"gameDate"`
	Annotations  []Annotation `json:"annotations"`
}

type Annotation struct {
	GameTime   string `json:"gameTime"`
	Label      string `json:"label"`
	Position   string `json:"position"` // milliseconds into the half, as a STRING in the real files
	Team       string `json:"team"`
	Visibility string `json:"visibility"`
}

func ParseLabels(r io.Reader) (Labels, error) {
	var l Labels
	if err := json.NewDecoder(r).Decode(&l); err != nil {
		return Labels{}, fmt.Errorf("parsing labels: %w", err)
	}
	return l, nil
}

// MatchMeta is the match-level data ingest needs; teams and date come from the JSON, not
// from the folder name (DATASET.md).
type MatchMeta struct {
	Home, Away string
	Date       time.Time
}

var errBadMeta = errors.New(ReasonBadMatchMetadata)

func (l Labels) Meta() (MatchMeta, error) {
	home, away := strings.TrimSpace(l.GameHomeTeam), strings.TrimSpace(l.GameAwayTeam)
	if home == "" || away == "" {
		return MatchMeta{}, fmt.Errorf("%w: home or away team missing", errBadMeta)
	}
	d, err := ParseGameDate(l.GameDate)
	if err != nil {
		return MatchMeta{}, fmt.Errorf("%w: %v", errBadMeta, err)
	}
	return MatchMeta{Home: home, Away: away, Date: d}, nil
}

// ParseGameDate reads "DD/MM/YYYY - HH:MM". Only the date is kept: the kickoff time has no
// timezone in the data, and matches.match_date is a DATE.
func ParseGameDate(s string) (time.Time, error) {
	t, err := time.Parse("02/01/2006 - 15:04", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("gameDate %q: want DD/MM/YYYY - HH:MM", s)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
}

var errBadAnnotation = errors.New(ReasonBadAnnotation)

// ParseEvent returns the half and the position in ms. Only the half is taken from gameTime
// ("1 - 27:36"); the timestamp comes from position, which is exact (DATASET.md rule 1).
func ParseEvent(a Annotation) (half, positionMS int, err error) {
	gt := strings.TrimSpace(a.GameTime)
	switch {
	case strings.HasPrefix(gt, "1 "):
		half = 1
	case strings.HasPrefix(gt, "2 "):
		half = 2
	default: // "3 - …", "10 - …", "" — anything that is not exactly half 1 or 2
		return 0, 0, fmt.Errorf("%w: gameTime %q", errBadAnnotation, a.GameTime)
	}
	pos, err := strconv.Atoi(strings.TrimSpace(a.Position))
	if err != nil || pos < 0 || pos > math.MaxInt32 { // clips.position_ms is INT
		return 0, 0, fmt.Errorf("%w: position %q", errBadAnnotation, a.Position)
	}
	return half, pos, nil
}

// folderRe is the SoccerNet match folder grammar (DATASET.md). Greedy (.+) for the home team
// backtracks to the single " <n> - <m> " score separator, so names with digits parse.
var folderRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}) - (\d{2})-(\d{2}) (.+) (\d+) - (\d+) (.+)$`)

// FolderMatches is a consistency check only: the JSON is authoritative, a mismatch is logged.
func FolderMatches(folder, home, away string) bool {
	m := folderRe.FindStringSubmatch(folder)
	return m != nil && m[4] == home && m[7] == away
}
