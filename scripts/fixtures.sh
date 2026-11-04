#!/usr/bin/env bash
# Generates synthetic test media into testdata/ (gitignored). NDA media never reaches tests or CI.
#
# Matches mirror the real SoccerNet layout (DATASET.md): two half files with H.264 video, an
# audio track and an ASS subtitle stream, plus a Labels-v2.json. They are 320x180 rather than
# 720p only to keep the ingest tests fast — resolution does not change any ingest logic.
#
# Usage: scripts/fixtures.sh [--force]
set -euo pipefail

OUT="testdata/matches/test_league/2015-2016"
FORCE="${1:-}"
[[ "$FORCE" == "--force" ]] && rm -rf testdata

# Clips for the transcode tests (IMPLEMENTATION_PLAN §12 #3), shaped like an ingest mezzanine:
# MP4, keyframe every 2 s, AAC stereo 48 kHz. testsrc2 compresses to almost nothing, so "CBR"
# means -b:v = -minrate = -maxrate = -bufsize with nal-hrd=cbr (filler data). Tests still read
# the MEASURED bitrate, never these nominal ones. Each is generated only if missing.
# clip <file> <WxH> <video bps> <audio:yes|no>
clip() {
  local file="testdata/$1" size="$2" bps="$3" audio="$4"
  [[ -f "$file" ]] && return
  local -a in=(-f lavfi -i "testsrc2=size=$size:rate=25") map=(-map 0:v) a=()
  if [[ "$audio" == yes ]]; then
    in+=(-f lavfi -i "sine=frequency=440:sample_rate=48000")
    map+=(-map 1:a)
    a=(-c:a aac -b:a 128k -ac 2 -ar 48000)
  fi
  ffmpeg -nostdin -v error -y "${in[@]}" "${map[@]}" -t 60 \
    -c:v libx264 -preset veryfast -b:v "$bps" -minrate "$bps" -maxrate "$bps" -bufsize "$bps" \
    -x264-params nal-hrd=cbr -pix_fmt yuv420p -force_key_frames "expr:gte(t,n_forced*2)" \
    ${a[@]+"${a[@]}"} -movflags +faststart "$file" # bash 3.2 (macOS): an empty array trips set -u
}
mkdir -p testdata
clip clip_1080p.mp4 1920x1080 5000000 yes   # → 4 rungs
clip clip_720p_low.mp4 1280x720 1500000 yes # → 3 rungs
clip clip_576p.mp4 1024x576 1500000 yes     # → 2 rungs, starts at 480p (§10 example D)
clip clip_noaudio.mp4 640x360 800000 no     # → terminal no_audio_stream
# → probe fails. Zeros, not random bytes: ffprobe 6.1 (CI's Ubuntu) accepts ~0.5% of random 4 KiB
# files as LRC lyrics (a subtitle stream, exit 0), which turns probe_failed into no_video_stream.
head -c 4096 /dev/zero > testdata/clip_corrupt.mp4

if [[ -f "$OUT/.complete" ]]; then
  echo "fixtures already present (use --force to regenerate)"
  exit 0
fi
mkdir -p "$OUT"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/sub.ass" <<'EOF'
[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,1,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,fixture subtitle
EOF

# half <file> <audio:yes|no>: 180 s, 25 fps, like a (very short) broadcast half.
half() {
  local file="$1" audio="$2"
  local -a in=(-f lavfi -i "testsrc2=size=320x180:rate=25") map=(-map 0:v)
  local next=1 # index of the next ffmpeg input
  if [[ "$audio" == yes ]]; then
    in+=(-f lavfi -i "sine=frequency=440:sample_rate=48000")
    map+=(-map "$next:a")
    next=$((next + 1))
  fi
  in+=(-i "$TMP/sub.ass")
  map+=(-map "$next:s")
  ffmpeg -nostdin -v error -y "${in[@]}" "${map[@]}" -t 180 \
    -c:v libx264 -preset veryfast -crf 35 -pix_fmt yuv420p -c:a aac -b:a 64k -c:s ass "$file"
}

# 1. Main match: the happy path and most rejections (IMPLEMENTATION_PLAN §12 #1–#5).
#    Team names contain digits on purpose (folder grammar edge case).
M="$OUT/2015-08-22 - 15-00 Mainz 05 2 - 1 Hannover 96"
mkdir -p "$M"
half "$M/1_720p.mkv" yes
half "$M/2_720p.mkv" yes
cat > "$M/Labels-v2.json" <<'EOF'
{
  "UrlLocal": "test_league/2015-2016/2015-08-22 - 15-00 Mainz 05 2 - 1 Hannover 96/",
  "gameHomeTeam": "Mainz 05",
  "gameAwayTeam": "Hannover 96",
  "gameDate": "22/08/2015 - 15:00",
  "gameScore": "2 - 1",
  "annotations": [
    {"gameTime": "1 - 01:00", "label": "Goal", "position": "60000", "team": "home", "visibility": "visible"},
    {"gameTime": "2 - 01:00", "label": "Goal", "position": "60000", "team": "away", "visibility": "visible"},
    {"gameTime": "1 - 00:05", "label": "Goal", "position": "5000", "team": "home", "visibility": "visible"},
    {"gameTime": "1 - 02:50", "label": "Goal", "position": "170000", "team": "home", "visibility": "visible"},
    {"gameTime": "1 - 01:40", "label": "Goal", "position": "100000", "team": "away", "visibility": "not shown"},
    {"gameTime": "2 - 02:00", "label": "Yellow card", "position": "120000", "team": "home", "visibility": "visible"}
  ]
}
EOF

# 2. A match whose halves have no audio → no_audio_stream.
M="$OUT/2015-08-29 - 15-00 Team A 0 - 1 Team B"
mkdir -p "$M"
half "$M/1_720p.mkv" no
half "$M/2_720p.mkv" no
cat > "$M/Labels-v2.json" <<'EOF'
{
  "UrlLocal": "test_league/2015-2016/2015-08-29 - 15-00 Team A 0 - 1 Team B/",
  "gameHomeTeam": "Team A", "gameAwayTeam": "Team B",
  "gameDate": "29/08/2015 - 15:00", "gameScore": "0 - 1",
  "annotations": [
    {"gameTime": "1 - 01:00", "label": "Goal", "position": "60000", "team": "away", "visibility": "visible"}
  ]
}
EOF

# 3. Second half missing (like a partial download), a bad half number, a position past the
#    end of the file → missing_video, bad_annotation, position_out_of_range.
M="$OUT/2015-09-05 - 15-00 Team C 1 - 0 Team D"
mkdir -p "$M"
half "$M/1_720p.mkv" yes
cat > "$M/Labels-v2.json" <<'EOF'
{
  "UrlLocal": "test_league/2015-2016/2015-09-05 - 15-00 Team C 1 - 0 Team D/",
  "gameHomeTeam": "Team C", "gameAwayTeam": "Team D",
  "gameDate": "05/09/2015 - 15:00", "gameScore": "1 - 0",
  "annotations": [
    {"gameTime": "2 - 01:00", "label": "Goal", "position": "60000", "team": "home", "visibility": "visible"},
    {"gameTime": "3 - 00:10", "label": "Goal", "position": "10000", "team": "home", "visibility": "visible"},
    {"gameTime": "1 - 16:39", "label": "Goal", "position": "999999", "team": "home", "visibility": "visible"}
  ]
}
EOF

# 4. Labels without gameHomeTeam → bad_match_metadata (rejected before any video is read).
M="$OUT/2015-08-30 - 15-00 X 0 - 0 Y"
mkdir -p "$M"
cat > "$M/Labels-v2.json" <<'EOF'
{"UrlLocal": "test_league/2015-2016/2015-08-30 - 15-00 X 0 - 0 Y/", "gameAwayTeam": "Y",
 "gameDate": "30/08/2015 - 15:00", "annotations": []}
EOF

# 5. hf_hub_download metadata, which ingest must skip (DATASET.md).
mkdir -p "testdata/matches/.cache/huggingface"
echo '*' > "testdata/matches/.cache/huggingface/.gitignore"

touch "$OUT/.complete"
echo "fixtures written to testdata/"
