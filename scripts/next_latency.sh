#!/usr/bin/env bash
# p50 / p95 of GET /next over N sequential warm requests, through the whole HTTP stack (gate,
# auth, rate limiter) — plan §11 LATENCY. Needs a local API in AUTH_MODE=dev; the dev user
# should have ≥ COLD_START_THRESHOLD loved/not_for_me ratings, or every pick is cold.
#
#   make latency                 # BASE=http://localhost:8080 N=1000
#
# Paced at ~4.5 req/s to stay under the `default` rate limit (300/min per IP); the pause is
# not part of the measured time.
set -euo pipefail
BASE=${BASE:-http://localhost:8080}
N=${N:-1000}
: "${INVITE_CODE:?set INVITE_CODE (make latency takes the first code in INVITE_CODES)}"

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
jar=$dir/jar

curl -sf -c "$jar" -X POST "$BASE/api/v1/gate" -d "{\"code\":\"$INVITE_CODE\"}" >/dev/null
session=$(curl -sf -b "$jar" -X POST "$BASE/api/v1/sessions" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)

next() {
  curl -s -b "$jar" -o "$dir/body" -w '%{http_code} %{time_total}\n' "$BASE/api/v1/sessions/$session/next"
}
for _ in $(seq 20); do next >/dev/null; sleep 0.21; done # warm-up: connections, plans, caches

: >"$dir/times"
: >"$dir/modes"
for i in $(seq "$N"); do
  read -r code secs < <(next)
  if [ "$code" != 200 ]; then
    echo "request $i: HTTP $code" >&2
    cat "$dir/body" >&2
    exit 1
  fi
  echo "$secs" >>"$dir/times"
  grep -o '"mode":"[a-z]*"' "$dir/body" | cut -d'"' -f4 >>"$dir/modes"
  sleep 0.21
done

sort -n "$dir/times" | awk -v n="$N" '
  { t[NR] = $1 * 1000 }
  END {
    p50 = t[int((n * 50 + 99) / 100)]; p95 = t[int((n * 95 + 99) / 100)] # nearest rank
    printf "requests %d   p50 %.1f ms   p95 %.1f ms   max %.1f ms\n", n, p50, p95, t[n]
  }'
printf 'modes   '
sort "$dir/modes" | uniq -c | awk '{ printf "%s %d   ", $2, $1 } END { print "" }'
