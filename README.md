# GoalStream

A football highlights streaming platform in Go: clips cut from full broadcast matches, stored in
object storage, delivered by presigned URLs (later adaptive HLS), selected by a content-based
recommender.

Dataset used under research NDA (SoccerNet), not publicly redistributed. No media or annotation
file is ever committed to this repository.

Design and plan live in local, untracked `docs/` (kept private alongside the dataset NDA).

## Milestones

Each milestone adds a section here: what it does, how it works, what I learned, and what I chose
not to build and when that choice would be wrong.

### M1 — skeleton, CI, gate, ingestion (`m1-ingest`)

**What it does.** `make ingest` walks SoccerNet match folders, cuts a 60-second clip around every
annotated goal, checks it, uploads it to object storage and indexes it in Postgres. The API lists
clips and hands out short-lived presigned URLs; the browser fetches the video straight from
storage. Every request passes a per-IP rate limiter and an invite gate. CI runs on every push and
the production API deploys only after CI is green.

**How it works.**

- *Ingest* (`cmd/ingest`, `internal/ingest`): labels → dedup pre-check (a re-run cuts nothing it
  already has) → ffmpeg **re-encode** of [−15 s, +45 s], never a stream copy, which would snap to a
  keyframe and can miss the goal → ffprobe + full-decode QC gate → vocab slots → 128-dim
  embedding → **upload first, then insert** the row and its audit entry in one transaction. A crash
  between the two leaves an inert object, never a row pointing at nothing.
- *Never above the source*: the mezzanine's video `-maxrate` is (source total − 128k audio) × 0.98,
  measured on real clips to land at 0.98–0.995× the broadcast bitrate.
- *Tags* are hand-labelled: `tags_todo.csv` → `make ingest-tags` applies the sheet and rebuilds
  every embedding in one transaction; one bad line applies nothing.
- *API* (`cmd/api`): request ID → recover → 64 KiB body limit → per-IP rate limit → invite gate →
  auth → per-user rate limit → handler. `/play` returns `{type, url, expires_at}` and nothing else.
- *Rate limiting*: fixed window in Redis. Gate rules **fail closed** (503) when Redis is down;
  everything else fails open.
- *Invite gate*: `gs_invite` = id | exp | HMAC; removing an id from `INVITE_CODES` revokes every
  cookie it issued.

**Run it locally.**

```bash
cp .env.example .env && make up && make migrate
make fixtures && make test && make test-integration
make ingest                      # DATA_DIR=./data
make api                         # then, in another shell:
curl -c jar -X POST localhost:8080/api/v1/gate -d '{"code":"let-me-in"}'
curl -b jar 'localhost:8080/api/v1/clips?limit=3'
curl -b jar localhost:8080/api/v1/clips/<id>/play
```

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| A streaming proxy: the API never touches media bytes | serve-time control is needed: per-byte metering, mid-stream revocation, DRM |
| Revocable playback URLs: a presigned URL works for its 15 minutes, gate or no gate | clips must be revocable instantly, or the URL scope grows past one object |
| A sliding-window or token-bucket limiter: fixed windows allow a 2× burst at a boundary | an endpoint where a boundary burst does real damage (expensive writes, paid APIs) |
| Rate limiting by the real client IP behind Render's proxy (`CLIENT_IP_HEADER` unset) | there is more than one real user — today every request shares one per-IP bucket |
| An orphan sweep for objects whose row insert failed | a crash rate where orphaned mezzanines cost real storage |
| Automated shot-type tagging (tags are hand-labelled) | a trustworthy vision model exists and the catalogue is too large to label by hand |
| Branch protection (needs a paid plan on a private repo): "never merge red" is a rule | more than one person merges into `main` |
| Embedding v2: v1 holds 64 teams; ingest aborts loudly when full | the catalogue grows past 64 scoring teams |

### M2 — viewing UI (`m2-viewer`)

**What it does.** A mobile-first React app: enter the invite code, start a session, watch a clip,
rate it (loved / okay / not for me) or skip it, and get a summary at the end. Clips are picked at
random among the ones you have not rated yet (the recommender arrives in M4). The frontend is
deployed on Vercel; it reaches the API on Render through a same-origin `/api/*` rewrite.

**How it works.**

- *Sessions* (`POST /sessions`, `/end`, `/summary`): every lookup is `WHERE id AND user_id`, so
  someone else's session is a 404, never a 403. Ending is a guarded UPDATE on `ended_at IS NULL`:
  a second call is a no-op.
- *Feedback* (`POST /feedback`, `internal/recommend/rate.go`): an append-only log, the current
  rating is the latest row. **Idempotent by state** — the same response twice appends nothing, so
  double taps and retries are harmless. Every rating change of one user is serialized by a lock
  on the user's row taken first; without it, two concurrent requests both read "no rating yet" and
  both append (a test proves it: 12–15 rows instead of 10 with the lock removed).
- *seen_clips* is written by feedback, never by `/play`: being handed a URL is not watching.
- *Next* (`GET /sessions/{id}/next?exclude=`): eligible = clips − seen − this session's skips.
  Nothing left is `200 {clip:null, exhausted:true}`, not an error.
- *Player* (`web/src/Player.tsx`, decisions in `playback.ts`): an expired presigned URL is
  re-fetched and playback resumes where it was; a real failure retries `/play?format=mp4` once;
  a second failure skips the clip silently. The skip list lives in the client, capped at 100.
- *Same origin*: the browser only ever calls its own `/api/v1/*` (Vite proxy locally, Vercel
  rewrite in production), so there is no CORS and the cookies stay first-party.

**Run it locally.**

```bash
make up && make api              # shell 1
cd web && npm ci && cd ..        # once
make fe                          # shell 2 → http://localhost:5173, invite code from .env
```

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| A server-side skip list: skips live in component state and vanish on reload | sessions span tabs or devices, or a reload must not resurface skipped clips |
| Resumable sessions: a reload starts a new one | users routinely reload mid-session (flaky mobile networks) |
| Playback telemetry beyond `watched_full` and `replay_count` | the recommender needs dwell time or drop-off points |
| A scalable random pick: every eligible row is loaded and one is chosen in Go | the catalogue passes ~1000 clips (M4 moves selection into SQL) |
| Browser end-to-end tests: only the player's decision logic is unit-tested | the UI flows change often, or more than one person ships frontend code |
| Closing ended sessions to feedback: a retry may land after `/end` | a session bounds something real — a quota, billing, a time-boxed experiment |
| Viewing in production: `AUTH_MODE=closed` until sign-in exists (M4) | — this is a staging step, not a trade-off |

### M3 — HTTP Range requests, educational only (`m3-range-edu`)

**What it does.** `GET /api/v1/stream/{id}` serves a clip's bytes **through the API**, with
hand-written HTTP Range support. It exists to learn what object storage already does for free
— it is mounted only when `APP_ENV=dev`, sits behind the gate and auth, and the player never
uses it. Production playback stays on presigned URLs (M1).

**How it works.**

- *Seeking already worked* without any of this: the player reads the MP4 index (`+faststart`
  puts it first), turns a timestamp into a byte offset and sends `Range: bytes=N-` straight to
  storage, which answers 206. The API handed out a URL and left the room.
- *Part 1* (`http.ServeContent`) showed the target behaviour — and its cost: every request pulled
  the whole ~60 MB clip into the API's memory, even for 100 bytes.
- *Part 2* (`internal/httpx/range.go`, `internal/handlers/stream.go`): HEAD the object for its
  size → parse `Range` → fetch **only that slice** with `GetObjectInput.Range` → 206 with
  `Content-Range`/`Content-Length`; no header → 200; unsatisfiable or malformed → 416 with
  `Content-Range: bytes */<size>` and an empty body.
- *Tested against an oracle*: the same requests go to the handler and to `http.ServeContent`;
  status, headers and body must match. Where the RFC or the plan is stricter, the expectation is
  written by hand: multi-range → 200 whole body; `bytes=-0` and `bytes=+1-2` → 416 (ServeContent
  accepts both); every 416 states the size.
- *Even Part 2 is still a proxy*: memory is flat, but every byte a viewer watches crosses the
  API's network. That is the whole lesson.

**When proxying media IS justified.** When control is needed at serve time, not just at
hand-out time: revoking access mid-stream, metering bytes per user for billing, DRM licence
checks, or watermarking per viewer. Then the bandwidth bill is the price of that control, and
the proxy belongs at the edge (a CDN worker), not in the API process.

**Run it locally.**

```bash
APP_ENV=dev make api
curl -c jar -X POST localhost:8080/api/v1/gate -d '{"code":"let-me-in"}'
curl -b jar -s -o /dev/null -D - -H 'Range: bytes=0-99' localhost:8080/api/v1/stream/<id>   # 206
curl -b jar -s -o /dev/null -D - -H 'Range: bytes=999999999-' localhost:8080/api/v1/stream/<id>  # 416
```

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| The route in production: it exists only under `APP_ENV=dev` | serve-time control (metering, revocation, DRM) becomes a requirement |
| Multi-range (`multipart/byteranges`): several ranges get 200 with the whole body | a client that really sends multi-range, e.g. a PDF viewer fetching scattered pages |
| Conditional requests (`ETag`, `If-Range`, `Last-Modified`) | the bytes behind a key can change — they cannot here: originals are immutable |
| `HEAD /stream/{id}` | a client probes the size before ranged reads instead of reading `Content-Range` |
| A longer `WriteTimeout`: the server's 30 s can cut a whole-clip download on a slow link | the route ever leaves localhost |
| Reusing one storage round trip: each request does HEAD then GET | per-request latency to storage matters — the size could come from Postgres instead |
