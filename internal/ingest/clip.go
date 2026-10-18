package ingest

import (
	"errors"
	"fmt"

	"github.com/vinhpnguyen123/goalstream/internal/transcode"
)

// MatchMinute is the football minute: 14:22 into the first half is the 15th minute; the
// second half starts at 46. First-half injury time yields 46, 47… which overlaps the second
// half — harmless, because identity is (half, position_ms), never the minute.
func MatchMinute(half, positionMS int) int {
	m := positionMS/60_000 + 1
	if half == 2 {
		m += 45
	}
	return m
}

// Title renders e.g. "Chelsea vs Burnley — Goal 15'", with injury time as 45+1' / 90+4'.
// Built entirely from data: no team, league or language is known to the code.
func Title(home, away, label string, half, minute int) string {
	shown := fmt.Sprint(minute)
	switch {
	case half == 1 && minute > 45:
		shown = fmt.Sprintf("45+%d", minute-45)
	case half == 2 && minute > 90:
		shown = fmt.Sprintf("90+%d", minute-90)
	}
	return fmt.Sprintf("%s vs %s — %s %s'", home, away, label, shown)
}

type Window struct{ StartMS, DurationMS int }

var errOutOfRange = errors.New(ReasonPositionOutOfRange)

// CutWindow places [position−pre, position+post] inside the half file. It clamps at both
// ends — a goal in the first seconds or the last seconds yields a shorter clip, which the QC
// gate then judges. A position at or past the end of the file is an annotation error.
func CutWindow(positionMS, halfMS, preMS, postMS int) (Window, error) {
	if positionMS >= halfMS {
		return Window{}, fmt.Errorf("%w: position %d ms, half is %d ms", errOutOfRange, positionMS, halfMS)
	}
	start := max(0, positionMS-preMS)
	end := min(halfMS, positionMS+postMS)
	return Window{StartMS: start, DurationMS: end - start}, nil
}

// QC decides whether a cut clip may be ingested. "" means it passes. Order matters only for
// which single reason is reported; a clip failing several checks is rejected either way.
func QC(p transcode.Probe, minMS int, decodeErr error) string {
	switch {
	case !p.HasVideo:
		return ReasonNoVideoStream
	case !p.HasAudio:
		return ReasonNoAudioStream
	case p.DurationMS < minMS:
		return ReasonTooShort
	case decodeErr != nil:
		return ReasonDecodeFailed
	}
	return ""
}

// MezzanineCap is the -maxrate for the mezzanine (decision D15): never above the source
// half's own bitrate, and never above the configured ceiling. A source that reports no
// bitrate falls back to the ceiling alone.
func MezzanineCap(sourceBps, ceilingBps int) int {
	if sourceBps <= 0 {
		return ceilingBps
	}
	return min(sourceBps, ceilingBps)
}
