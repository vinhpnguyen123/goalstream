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

### M4 — auth and recommender (`m4-auth-reco`)

**What it does.** Real accounts replace the dev stub: register and sign in behind the invite gate,
stay signed in across reloads, sign out. Every rating now teaches the system what you like, and
`/next` picks from that instead of at random — mostly clips close to what you loved and away from
what you didn't, with a share of deliberately different ones. A history screen lists your current
ratings and lets you change any of them. Production runs `AUTH_MODE=jwt` with one registered
account.

**How it works.**

- *Passwords* (`internal/auth/password.go`): argon2id with library defaults, 10–128 characters.
  An unknown email still runs a comparison against a fixed dummy hash, so response time does not
  reveal which emails exist.
- *Access token*: JWT HS256, 15 minutes, held **in memory only** in the browser — never
  localStorage. Verification pins the algorithm, so a token claiming `none` or RS256 is refused.
- *Refresh token* (`internal/auth/refresh.go`): 32 random bytes in an `HttpOnly; SameSite=Strict`
  cookie scoped to `/api/v1/auth`, stored as a SHA-256 hash. Every refresh rotates it within a
  *family*. Replaying a rotated token within 30 s is a tab race (two tabs, React StrictMode) and
  gets a successor; later, it is treated as theft and the whole family is revoked and audited.
  The client single-flights refresh, so concurrent 401s share one rotation.
- *One lock per family*: rotation, logout and theft revocation all take an advisory lock on the
  family first. Without it, a logout racing a rotation revoked the old token but missed the
  successor the rotation was inserting — a deterministic test held a rotation open before COMMIT
  and showed the successor still refreshing after logout.
- *Admin*: `users.is_admin`, granted only by `make admin EMAIL=…`, read from Postgres on every
  request; a non-admin gets 404, not 403.
- *Taste* (`internal/recommend/rate.go`): each user has a positive and a negative centroid — the
  running mean of the vectors of clips they currently love / dislike. A rating moves one vector:
  subtract it from the old rating's mean, add it to the new one's. One transaction, serialized
  per user by locking the `user_taste` row first. Removing the last vector makes the mean NULL,
  not zero.
- *A bug the concurrency test found*: `created_at` defaulted to `now()`, which is the
  transaction's *start*. A request that started first but waited on the lock appended a row that
  sorted before the one it followed, so the next request read a stale "current rating" and
  removed the vector from the wrong mean. Rows are now stamped under the lock, strictly after
  the previous one.
- *Rebuilds* (`internal/recommend/rebuild.go`): when clip vectors change (`make ingest-tags`,
  `make reembed`), every centroid is recomputed from current ratings with pgvector's `AVG`, in
  the same transaction, under a lock that ratings take in shared mode. The same rebuild runs for
  one user the first time they rate after this milestone, so ratings from M2 are not lost.
- *Selection* (`internal/recommend/next.go`):
  - fewer than 5 loved + not-for-me ratings → *cold*: uniform random, and the centroids are not
    even read (a query counter in the test proves it). Rating everything "okay" stays cold —
    it says nothing directional.
  - otherwise 70 % *exploit*: the 50 nearest eligible clips by cosine distance in SQL, re-ranked
    in Go by `cos(v, pos) − 0.5·cos(v, neg)` — one `ORDER BY` cannot express distance to two
    vectors. Dislikes only → the clips farthest from what you disliked.
  - 30 % *explore*: uniform among eligible clips less similar to your taste than the median.
  - randomness comes from an injected RNG, so the tests are seeded and deterministic.

**Run it locally.**

```bash
make up && make api && make fe   # AUTH_MODE=dev skips sign-in; AUTH_MODE=jwt + JWT_SECRET for real accounts
make reembed                     # rebuild every embedding and every centroid
make latency                     # p50/p95 of GET /next (AUTH_MODE=dev, a user with ≥ 5 ratings)
```

**Latency of `GET /next`** — 1000 sequential warm requests through the whole HTTP stack (gate,
auth, rate limiter), local Postgres 16 + pgvector 0.8.7, Apple M1, measured 2026-10-23:

| Catalogue | p50 | p95 | max | Modes served |
|---|---|---|---|---|
| 21 clips | 6.6 ms | 18.2 ms | 231.9 ms | 695 exploit · 305 explore |
| 88 clips (2026-10-24, M4.5) | 7.2 ms | 16.7 ms | 2867.7 ms (one outlier) | 712 exploit · 288 explore |

Four times the catalogue moved p50 by 0.6 ms: below ~1000 clips the exact scan is not the cost.

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| A cap on concurrent argon2id hashes (64 MB each) | registration is public, without the invite gate in front of it |
| `/auth/*` outside jwt mode: dev and closed mount no auth routes | another mode needs to issue or revoke tokens |
| An absolute lifetime for a refresh family: expiry slides 7 days with each rotation | a policy requires re-authentication every N days regardless of activity |
| An audit trail for admin grants (`make admin` is SQL) | admins are granted through the API or by more than one person |
| Proactive refresh before the access token expires: refresh happens on the first 401 | a request cannot be retried safely, or the extra round trip every 15 min is felt |
| Logout synced across tabs: another tab learns at its next 401 | several people share one browser |
| Per-family lock rows: the advisory lock key is `hashtext(family_id)`, so two families can collide | refresh volume is high enough that colliding families waiting on each other is measurable |
| Tag-based explanations ("because you liked headers"): reasons are generic | users need to trust or correct why a clip was chosen |
| A vector index: stage 1 is an exact scan | the catalogue passes ~1000 clips (HNSW) |
| Ties at the explore median: below-median is strict, so mostly-identical vectors empty the pool (→ exploit) | many clips share an embedding, e.g. untagged clips of one team |
| Incremental rebuilds: a rebuild recomputes every user under an exclusive lock that pauses rating | there are enough ratings that `make reembed` takes noticeable time |
| A maintained current-ratings table: `GET /ratings` computes every current rating, then pages | a user has thousands of ratings |
| History reachable mid-session: only from the start and summary screens | users want to fix a rating while watching |
| A single centroid per sign: it cannot represent multi-modal taste | a user loves two unrelated kinds of goals and the mean lands between them |

### M4.5 — Phase A content in production (`m4.5-deployed`)

**What it does.** The production app now has the full catalogue: 88 goal clips from 23 matches
(2016-17, five competitions), as MP4 in Cloudflare R2, indexed in Render Postgres. Production is
single-viewer under the dataset NDA — one invite code, one account — and stays MP4; adaptive
streaming (Phase B) is built and demonstrated locally only.

**How it works.**

- *MinIO → R2 is config only.* `cmd/ingest` runs on my laptop against production with a
  gitignored `.env.prod`; not a line of Go changed. `scripts/prod.sh` runs a command with **only**
  `.env.prod` in its environment (`env -i`): the Makefile exports the local `.env`, so a variable
  missing from `.env.prod` would otherwise silently fall back to its local value — `APP_ENV=dev`,
  the local bucket. Now it fails config instead.
- *The production database admits no external connection.* For an ingest I allow-list my IP for
  the duration of the run and remove it after.
- *Free-tier Postgres expires after 30 days.* It is kept by `make prod-dump` before expiry and
  `make prod-restore` into the new one — not by re-ingesting, which would mint new clip ids,
  orphan every object in R2 and lose tags, ratings and the account. The dump/restore run in the
  `pgvector:pg16` image to match the server: a newer local `pg_dump` writes settings PG16 rejects.
- *Tags.* `penalty`, `late_goal` (≥ 85'), `equaliser` and `winner` are derived from the
  annotations (a missed penalty followed by a later goal stays untagged). The shot-type tags
  (`header`, `long_range`, `volley`, …) were **assigned by an AI model from extracted frames,
  not by hand, and are unverified** — `counter`, `team_move` and `deflection` are the least
  reliable.

**Baselines for Phase B** — time-to-first-frame (src assigned → first `playing`, or from the tap
when autoplay is blocked) and stalls (`waiting` after the first frame, seeks excluded), measured
with `scripts/ttff.js` pasted into the console. Warm API (the free Render instance sleeps; a cold
start adds up to a minute to the first request and is not a delivery property).

| Device | Network | Clips | TTFF p50 | TTFF p95 | Stalls |
|---|---|---|---|---|---|
| Desktop Chrome (Mac, home broadband, 2026-10-25) | no throttling | 10 | 618 ms | 1180 ms (= max at n=10) | 0 |
| Desktop Chrome (same machine, same day) | DevTools "Slow 4G" | 10 | 3540 ms | 3957 ms (= max) | 28 — every clip, 2–4 each, 52.4 s total |

> **Why Phase B exists — measured, not assumed.**
>
> On a fast link MP4 is fine: first frame in ~0.6 s, zero stalls. On Slow 4G **every one of the
> 10 clips stalled, 2–4 times each, 52 seconds of frozen video in total**, and the first frame
> took ~5.7× longer (p50 618 ms → 3540 ms).
>
> The cause is structural, not a bug: each clip exists as **one** MP4 at the broadcast's bitrate
> (~2.8–3.2 Mbps). When the link is slower than the file, the player has no smaller version to
> switch to — it can only stop and wait. More caching, a CDN or a faster server do not change
> that; the bytes per second of video are fixed.
>
> Phase B (M5–M9) removes that constraint on the running app, per clip and reversibly: an
> HLS ladder of renditions (never above the source, ceilings 5.0 / 3.0 / 1.4 / 0.7 Mbps) cut into
> 4-second segments, so the player drops to a rung that fits the link and trades picture for
> continuity. **The success criterion is this table:** re-run `scripts/ttff.js` unchanged on
> Slow 4G against the HLS version of the same clips — stalls should approach zero and the first
> frame should arrive sooner.

Not measured: iPhone (Wi-Fi, cellular), and a full session on a real phone — the M4.5
"works on a phone over the public internet" criterion was waived (2026-10-25). The only phone
check so far is a phone-sized viewport in DevTools (M2).

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| Ingest in CI or on a server: it runs from my laptop | more than one person ingests, or the source data cannot sit on a laptop |
| A paid database: free tier + monthly dump/restore | missing a dump loses real users' data, or downtime during restore matters |
| Playback telemetry (real-user TTFF and stalls): baselines are measured by hand | there are real viewers whose experience I cannot sit next to |
| A CDN or custom domain in front of R2: presigned URLs hit R2 directly | viewers are far from R2's region, or the same clips are watched often enough to cache |
| Hand-verified shot-type tags | recommendations are judged on tag quality, or tags are shown to users |
| An always-on API instance: the free one sleeps | the cold start (up to a minute) is felt by someone other than me |

### M5 — HLS for one clip (`m5-hls-one-clip`)

**What it does.** A clip can now be delivered as adaptive HLS — 720p / 480p / 360p, 4-second
segments — instead of one MP4, **per clip and reversibly**, on the running app: every other clip
keeps playing as MP4, and an admin rollback puts any clip back on its original instantly. Local
only: Phase B is never deployed (production stays MP4; see M4.5 for why).

**How it works.**

- *The ladder* (`internal/transcode/ladder.go`) is derived, never fixed: never above the source's
  measured video bitrate, never above the source height, capped per height (5.0 / 3.0 / 1.4 /
  0.7 Mbps — our "source" is a CRF mezzanine whose own bitrate is an inflated cap), halving per
  rung, stopping below 300 kbps. On the real catalogue: a 2.5 Mbps 720p clip →
  720p@2524k / 480p@1262k / 360p@631k; four clips above 3 Mbps hit the 720p ceiling.
- *Encoding* (`hls.go`): keyframes forced every 2 s **by time** (not `-g 48`, which is 1.92 s at
  25 fps), so segment N starts at the same instant in every rung and the player can switch
  mid-stream cleanly — tested by comparing each segment's first PTS across rungs. `-maxrate` and
  `-bufsize` bound the peak, so the master's `BANDWIDTH` is a true upper bound.
- *One prefix per attempt* (`job.go`): every claim mints a fresh `output_version`, which is both
  the attempt's private prefix `hls/<version>/` and its fencing token. `master.m3u8` is uploaded
  **last**: no master, no version. A crashed attempt leaves an orphan nobody points at.
- *The fenced commit* is the only way a version goes live: one transaction that first moves the
  job to `done` **guarded by this attempt's `output_version`**, then records renditions and
  points `clips.hls_version` at the version. A reaped, cancelled or superseded attempt matches
  0 rows and publishes nothing. The test that proves it makes the zombie commit *while* the next
  attempt is still processing — with only the status in the guard, that test fails.
- *Rollback* (`POST /admin/clips/{id}/delivery`, `admin.go`): one transaction that cancels the
  clip's active job (whose commit is then fenced out — rollback always wins) and flips the clip
  to mp4 or to an older committed version. Rolling back to mp4 keeps `hls_version`, so the version
  just left can be re-promoted. Admin routes exist only outside production.
- *Delivery*: `/play` presigns only `master.m3u8`; the player resolves renditions and segments
  relative to it, unsigned. Locally, a MinIO policy allows anonymous reads of `clips/*/hls/*` and
  nothing else (originals stay presigned-only, MinIO binds 127.0.0.1) — tested, and never applied
  to R2. `?format=mp4` always returns the original: the player's structural fallback.
- *Player*: hls.js is loaded only when an hls URL arrives (production never downloads its 575 kB);
  it starts at the rung the measured bandwidth allows. Any fatal HLS error — or the hls.js chunk
  failing to download — falls back to MP4 once, then skips. Safari on macOS has MSE, so it also
  uses hls.js; the browser's native HLS path runs only where MSE is missing (iOS < 17.1) and is
  covered by unit tests only — an iPhone cannot reach a 127.0.0.1 MinIO.

**Bugs the adversarial reviews found, and the tests that now pin them.**

| Bug | Fix |
|---|---|
| The commit ran outside the attempt timeout, and a failed commit skipped the failure handler: the job stayed `processing` forever (no reaper until M6) | The commit runs under the same deadline; any commit error goes through the fenced failure handler (`shutdown`, `timeout` or `commit_failed`). Tested with a SIGTERM right after the master upload and with a commit stuck behind a row lock |
| A job being created while a rollback ran was invisible to it, committed afterwards and later undid the rollback | Both take a per-clip advisory lock first. Tested deterministically with an uncommitted insert held open |
| A shutdown at the attempt limit failed the job, although shutdowns must not burn attempts | The limit check skips `shutdown` |
| A failed hls.js download left the player with nothing attached — no error, no fallback | A failed import means "no hls.js": native HLS, or an error that falls back to MP4 |

**Run it locally.**

```bash
make up && make fixtures
make api   # AUTH_MODE=dev                       # then, with the gate cookie:
curl -b jar -X POST localhost:8080/api/v1/admin/clips/<clip>/transcode          # 202 {job_id}
make worker JOB=<job_id>                                                          # ~30 s per clip
curl -b jar -X POST localhost:8080/api/v1/admin/clips/<clip>/delivery -d '{"source_type":"mp4"}'
```

**Measured: MP4 vs HLS on the same 10 clips** — local stack, Chrome DevTools "Slow 4G" with cache
disabled (measured link 1.1–1.3 Mbps), each clip played to 0:30 (at most 75 s), `scripts/ttff.js`
(it also records which rung every segment came from), 2026-11-01.

| | MP4 | HLS, starting at 720p | HLS, starting at the measured rung |
|---|---|---|---|
| TTFF p50 / max | **3.1 s** / 4.1 s | 9.6 s / 11.5 s | 7.1 s / 7.7 s |
| Stalls | 76 — every clip, 7–8 each | **0** | **0** |
| Time frozen | 409 s (~41 s per clip) | 0 | 0 |
| Rungs played | one 2.2–3.0 Mbps file | 720p×1 → 360p for the rest | 360p (2 clips tried 480p once, came back) |
| Waiting per clip (first frame + frozen) | **~44 s** | ~10 s | **~7 s** |

> **The M4.5 success criterion, honestly:** stalls went from 76 to **zero** — met. "The first
> frame should arrive sooner" — **not met**: HLS starts ~2× later than MP4. Starting at the first
> rung in the master (720p) meant downloading a whole 1.5 MB segment before the first frame;
> starting at the measured rung brought p50 from 9.6 s to 7.1 s, but HLS still needs four
> sequential requests (`/play`, master, media playlist, first segment — each paying Slow 4G's
> latency) and a whole 4-second segment before it shows a frame, where a faststart MP4 shows one
> after a few hundred KB. The trade is a later start for continuous playback: ~7 s of waiting per
> clip instead of ~44 s.

**How the player picks a rung** (hls.js 1.7.3 defaults): it measures throughput on every segment
and takes the highest rung whose peak `BANDWIDTH` fits 95 % of it (70 % to climb, so it does not
bounce; with ≥ 2 segments buffered it compares the average bitrate instead). On this catalogue:

| Rung | Peak `BANDWIDTH` | Link needed to hold it | Link needed to climb to it |
|---|---|---|---|
| 360p | 0.73 – 0.89 Mbps | ~0.8 – 0.95 Mbps | (start rung) |
| 480p | 1.36 – 1.67 Mbps | ~1.45 – 1.75 Mbps | ~1.95 – 2.4 Mbps |
| 720p | 2.66 – 3.50 Mbps | ~2.8 – 3.7 Mbps | ~3.8 – 5.0 Mbps |

At ~1.2 Mbps that is 360p; the two clips with the lightest 480p (average 1.22 Mbps, right at the
budget) tried it for one segment and dropped back. Below ~0.8 Mbps even 360p does not fit.

§12 #29 (adaptive switching): switching **down** is shown above on every clip (720p → 360p) and
both ways on two clips (360p → 480p → 360p). Switching back **up** when the link recovers was not
exercised by hand, and nothing was tested on an iPhone.

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| A queue, reaper and retries in the background: M5 runs one job by hand (`worker -once`) | it is M6 |
| Deleting old versions and crashed-attempt orphans: they accumulate, inert | storage cost matters — then a sweep comparing prefixes to `renditions`, never auto-deleting the live one |
| A rung below 360p | viewers sit on links under ~0.8 Mbps, where even 360p stalls |
| Shorter segments or hls.js progressive loading to cut HLS's first frame | the first second matters more than the stalls after it (short clips, autoplay feeds) |
| Testing native HLS (iOS Safari) | iPhones are expected viewers — it needs the stream reachable off-localhost, i.e. M9's access control |
| Thumbnails in the UI: `/clips` returns `thumbnail_url`, nothing renders it | a clip list or a poster image is built |
| Deploying any of it | real viewers need adaptive streaming in production (M4.5) |

### M6 — async transcoding pipeline (`m6-pipeline`)

**What it does.** Transcoding runs in the background: `POST /admin/clips/{id}/transcode` queues a
job and returns at once, a long-running worker takes it from Redis, and the clip switches to HLS
when the job commits. The worker survives being killed at any point: a crash, a `kill -9`, a
deploy's SIGTERM, a lost Redis push or a flushed Redis never lose a job, never publish one twice,
and never change bytes a viewer may be playing. `GET /admin/clips/{id}/status` shows where a clip
points, its newest job and every committed version. Local only, like all of Phase B.

**How it works.** Redis is the fast path; Postgres is the record. Every queued job is a
`transcode_jobs` row, and anything Redis loses is rebuilt from Postgres.

- *Queue* (`internal/transcode/queue.go`): the API inserts the row, **then** `LPUSH`es its id —
  if the push fails the API still answers 202, the row is the job. The worker `BLMOVE`s ids from
  `transcode:queue` to `transcode:processing` (5 s blocks, so a shutdown is noticed between jobs;
  exponential backoff to 30 s while Redis is down), runs the M5 claim → attempt → fenced commit,
  and `LREM`s the entry whatever happened — Postgres says what comes next.
- *Duplicates are harmless by construction*: the claim is a guarded `UPDATE … WHERE status IN
  ('queued','retryable')`, so of two workers holding the same id exactly one gets the row; the
  other drops its entry.
- *Shutdown* (`Loop.Run`): SIGTERM stops new `BLMOVE`s; the job in hand runs on a context cancelled
  `SHUTDOWN_GRACE` later, which kills ffmpeg (`exec.CommandContext`) and records the job
  `retryable` with reason `shutdown` **and its attempt given back** — three deploys in a row must
  not fail a healthy job.
- *The reaper* (`reaper.go`), a goroutine in every worker, safe in several at once because each
  write is guarded:
  - **stuck** — `processing` for longer than `JOB_STUCK_AFTER` means the worker died: → `retryable`,
    or `failed` with `worker_crash` once attempts ≥ MAX. A crash counts as an attempt, otherwise a
    file that kills the worker loops forever. The worker refuses to start unless
    `JOB_STUCK_AFTER ≥ TRANSCODE_TIMEOUT + 2m`, so a healthy job always ends first; an unhealthy one
    (laptop asleep) can outlive it — that is the zombie, and the M5 fence makes it harmless.
  - **re-push** — `queued`/`retryable` rows untouched for `REQUEUE_AFTER` are pushed again, oldest
    first, one row at a time: bump `updated_at` in Postgres, **then** push (skipped if `LPOS` finds
    it already queued). This one sweep re-enqueues retries (`REQUEUE_AFTER` is the backoff), rescues
    failed pushes, and rebuilds the whole queue after a flush.
  - **hygiene** — processing-list entries whose row is not `processing` (a worker that committed
    and died before its `LREM`) are removed.
- *Status* (`transcode.Status`): one read-only repeatable-read snapshot, so the pointer, the job and
  the version list agree. Versions come from `renditions` — what fenced commits wrote — so orphan
  prefixes of dead attempts never show up.

**The crash, fencing and requeue tests are the milestone** (§12 #30–#37; all run against real
Postgres, Redis, MinIO and ffmpeg, the concurrent ones 3× under `-race`).

| Scenario | What the test does | What must hold |
|---|---|---|
| Crash after uploads / after the master / after the commit | the loop's goroutine dies (`runtime.Goexit`: nothing recorded, like a dead process) at each point, on a clip that is already live | the clip plays its previous version until a commit; a fresh worker + reaper finish the job; one `transcode_completed`; every object of every version that was ever live keeps its ETag |
| Real `kill -9` | builds `cmd/worker`, SIGKILLs its process group mid-ffmpeg | job left `processing`; a restarted worker's reaper requeues it; published on attempt 2 |
| Real SIGTERM | `SHUTDOWN_GRACE=0` mid-ffmpeg | exit 0 within 15 s, no ffmpeg left behind, `retryable` with attempts unchanged; a restart finishes it on attempt 1 |
| Zombie | attempt 1 pauses after writing its master, the reaper calls it stuck, attempt 2 publishes, attempt 1 wakes and commits | its commit matches 0 rows; the clip stays on attempt 2 |
| Poison pill | the worker crashes on every attempt | after 3 the reaper fails it `worker_crash`; never claimed again |
| Corrupt input | a 4 KiB random file, driven only by the loop and the reaper | 3 attempts, `failed` / `probe_failed`, error ≤ 8 KiB, clip still playable |
| Lost push / flushed Redis | a row with no queue entry; both lists deleted mid-run | every job completes, once |
| Duplicate delivery | the same id pushed twice, two workers | one claim, one commit |
| Two triggers at once | two concurrent `POST /transcode` | one 202, one 409 |

**Bugs the adversarial reviews found, and the tests that now pin them.**

| Bug | Fix |
|---|---|
| The re-push sweep reset the clock of *every* eligible row in one `UPDATE`, then pushed them one by one: a pass cut short (sweep deadline, Redis error) left the rest reset but unpushed, and an arbitrary order could starve them | Oldest first, one row at a time: bump, then push. A row the pass never reached keeps its old clock and is first next time. Tested with a Redis hook that cancels the pass after two pushes |
| The queue slice alone acknowledged retryable jobs with nothing to re-push them | Built and shipped together with the reaper; the recovery tests use no manual pushes |

Found by a test, accepted: the reaper's first sweep can re-push a row in the instant after a worker
took it but before it claimed it. The duplicate is dropped by whichever worker loses the claim.

**Run it locally.**

```bash
make up && make fixtures
make api                    # AUTH_MODE=dev
make worker                 # the loop + reaper; Ctrl-C = graceful (SHUTDOWN_GRACE)
curl -b jar -X POST localhost:8080/api/v1/admin/clips/<clip>/transcode   # 202 {job_id}
curl -b jar localhost:8080/api/v1/admin/clips/<clip>/status              # queued → processing → done
make worker JOB=<job_id>    # still there: one job, no Redis
```

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| A "not before" time on retries: a duplicate entry still in the queue lets a failed job retry before `REQUEUE_AFTER` | the backoff protects something real (a rate-limited upstream, a struggling disk) |
| One processing list per worker: all workers share `transcode:processing` | many workers with heavy churn, where hygiene's full scan and its BLMOVE-before-claim edge cost something |
| Putting back a job taken after SIGTERM: it waits for the next re-push | `REQUEUE_AFTER` is long and deploys are frequent |
| Alerts or metrics on `worker_crash` / `failed` jobs: only the audit log records them | jobs run unattended (M7's batch) and nobody reads the audit log |
| A queue namespace setting: tests that spawn the real worker isolate it in Redis database 15 | two environments share one Redis |
| Concurrency inside one worker: one job at a time per process | the catalogue is large enough that one CPU-bound worker cannot keep up — run more processes first |
| Kafka, or any broker with replay | several independent consumers or sustained high throughput (ARCHITECTURE.md) |

### M7 — migrate the catalogue (`m7-catalog-migrated`)

**What it does.** Every local clip now plays as HLS. The whole catalogue went through the M6
pipeline in two runs, "a few, verify, then the rest", while the app kept serving: a clip switches
from MP4 to HLS only at its own fenced commit, and `original.mp4` stays behind it as the fallback
and rollback target. An admin badge in the web header shows the progress (`mp4 N · hls N`), and a
verifier proves afterwards that every clip can still be played. Local only, like all of Phase B.

**How it works.**

- *Progress* — `GET /admin/stats` (`transcode.ReadStats`): clips per `source_type` and jobs per
  status, every key present even at 0, both counts from one read-only snapshot so a commit landing
  between them cannot show a clip as HLS while its job still looks active. The web badge calls it
  once per page load and hides on 404 — non-admins and production get 404 — so no `/me` endpoint
  was needed.
- *The batch* — `make batch [LIMIT=N]` (`ingest transcode-batch -wait`):
  - `Pending` lists the clips still to migrate, oldest first: MP4, **never** migrated
    (`hls_version IS NULL`) and no active job. A clip an admin rolled back to MP4 keeps its version
    and is left alone; a clip whose last job failed is listed again, so re-running retries it.
    Re-running is also how "the rest" works: it only ever picks up what is left.
  - `Enqueue` creates and pushes one job per clip with the same code as the admin endpoint — the
    row before the push. Because `Pending` is only a snapshot, each job's transaction re-checks the
    clip **after** inserting the job: the insert waits out a fenced commit in flight, and rollback
    takes the same per-clip lock, so nothing can change the clip between the check and the commit.
  - `-wait` polls the jobs every 5 s and prints both success rates. Any failed job → exit 1, so
    `make batch && make verify-hls` stops.
- *The verifier* — `make verify-hls` (`transcode.Verify`), for every clip:
  - `original.mp4` exists;
  - for HLS clips, the live version has its master and one playlist per rung, every segment each
    playlist **names** exists, and each playlist names as many segments as the commit recorded;
  - `ffprobe` opens the presigned master the way a player does (variants fetched unsigned, as
    hls.js does) and must see one video stream per rung, at the rung's height.

  ffprobe's errors echo the URL it was given, so query strings are redacted before printing.

**The measured result.** One worker on a MacBook (battery, lid open, `caffeinate`), 78 clips:

| Run | Jobs | Wall time | Success | First try |
|---|---|---|---|---|
| `make batch LIMIT=10` | 10 | 4 m 40 s | 10/10 | 10/10 |
| `make batch` (the rest) | 68 | 28 m 31 s | 68/68 | 68/68 |
| **Total** | **78** | **33 m 11 s** | **100 %** | **100 %** |

Per job (claim → commit): p50 25.1 s, p95 29.6 s, max 39 s. After the run: `mp4 0 · hls 88`,
264 renditions (3 rungs per clip), `make verify-hls` → 88 clips, 0 problems.

100 % is honest but narrow: every input was a 720p25 mezzanine that my own ingest had already cut
and passed through its QC gate, so the pipeline was only ever fed files it was built for. It says
nothing about arbitrary uploads; the failure paths are covered by the M6 crash tests, not by this
run.

**Bugs the adversarial reviews found, and the tests that now pin them.**

| Bug | Fix |
|---|---|
| `Pending` is a snapshot and `CreateJob` allows re-transcodes, so a clip migrated — or migrated *and rolled back* — between the listing and its turn got a job, undoing the rollback | The batch's job transaction re-checks the clip after the insert. Tested with the commit held open during the insert; moving the check before the insert makes that test fail |
| The batch exited 0 even when every job failed | Report first, then exit 1 on any failure |
| The verifier counted segment files in the folder: a renamed later segment kept the count right, and ffprobe only reads the first one, so a clip that stalls mid-playback passed | Read each rendition playlist; every segment it names must exist |

Found by a test: when a variant playlist is missing, ffprobe silently drops that stream and still
exits 0. An exit-code check would have passed it; comparing the heights it sees with the rungs
does not.

**Run it locally.**

```bash
make up && make api && make fe      # the badge shows mp4 N · hls N for an admin
caffeinate -i make worker           # keep the laptop awake: a sleep can make the reaper burn attempts
caffeinate -i make batch LIMIT=10   # a few; exit 1 if any failed
make verify-hls                     # every clip still plays; exit 1 on any problem
caffeinate -i make batch            # the rest
make verify-hls
```

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| A batch HTTP endpoint or an admin "migrate all" button: the batch is a CLI that reuses `CreateJob` | someone other than an engineer runs migrations, or the API must be the only write path |
| More than one worker: 78 clips took 33 minutes | the catalogue is in the thousands (≈ 7 h per 1000 clips at 25 s each) — run N worker processes first; the claim already makes that safe |
| A live-updating badge: fetched once per page load | the web UI becomes the operator's console during a migration |
| Decoding every segment in the verifier: it checks that every named segment exists and opens the first of each variant | segments can be corrupted in storage after being written (bit rot, partial overwrite) — then store checksums at commit, or decode each variant in full |
| Retrying rolled-back clips automatically: they stay MP4 until an admin re-transcodes them | rollbacks become routine rather than a deliberate decision |
| Cleaning up old and orphaned versions (decision D9): they accumulate | storage cost matters — then a reaper for prefixes that are neither live nor a rollback target |

### M8 — caching layer, local (`m8-cache`)

**What it does.** HLS playback now goes through an nginx cache in front of MinIO. The first
viewer of a segment pays the trip to storage; every later viewer of that segment is served from
nginx's disk. It is the role a CDN plays in production, demonstrated on localhost — Phase B is
never deployed.

**How it works.**

- *nginx* (`deploy/local/nginx/default.conf.template`, started by `make up`, `127.0.0.1:8081`)
  proxies `/<bucket>/clips/*/hls/*` only, GET/HEAD only. Anything else — originals included — is
  a 404 from nginx without reaching MinIO.
- *Everything under `hls/` is cached for 7 days, with no purge.* Every object under
  `hls/<version>/` is immutable by construction (one prefix per attempt, master written last).
  The only mutable thing is which version is live, and that is `clips.hls_version`, delivered by
  the uncached API. So **playlists need no short TTL**: a re-transcode or a rollback changes the
  URL the API hands out, never the bytes behind an old one — the same pattern as hashed asset
  filenames behind an uncached HTML page.
- *The cache key is `$uri`*, never the query string: a presign signature (or any cache-buster) in
  the query must not fragment the cache. The upstream request drops the query too.
- *Only a 200 is cached.* A segment that 404s now is served the moment it exists.
- *nginx's config is the whole policy*: upstream `Cache-Control`, `Expires`, `Set-Cookie` and
  `Vary` are ignored. MinIO sends `Vary: Origin` — honoured, a browser and curl would each get
  their own copy of every segment.
- *CORS* names one fixed origin (`CORS_ORIGIN`, the web app). MinIO echoes the request's Origin;
  cached, that would replay the first viewer's origin to everyone.
- *`proxy_cache_lock`*: a cold segment requested by many viewers at once reaches MinIO once.
- *The access log* records `$uri` only — never `$request` or `$args`, which could carry a
  signature. `make cache-stats [SINCE=10m]` reads it.
- */play* (`HLS_BASE_URL`, an origin, local only — refused at startup in prod): for an HLS clip
  it returns the unsigned `<HLS_BASE_URL>/<bucket>/<prefix>hls/<version>/master.m3u8`, so hls.js's
  relative requests for renditions and segments go through nginx too. `?format=mp4` and MP4 clips
  stay presigned against MinIO. No new exposure: `hls/` was already anonymous-read on 127.0.0.1
  since M5; M9 puts the invite cookie in front of it.

**The measured result.** 10 HLS clips, real hls.js in Chrome, each clip buffered to the end (a
viewer who watches all of it), cold cache at the start. Each pass is a new viewer: a per-run,
per-pass query string makes the browser's own cache miss, while nginx's key ignores it.

| Pass | Viewer | Rung hls.js chose | Requests | Hit rate | Bytes | Byte hit rate |
|---|---|---|---|---|---|---|
| 1 | desktop, cold cache | 1 probe segment at 360p, then 720p | 190 | 0 % | 220.4 MB | 0 % |
| 2 | desktop, same 10 clips | the same | 190 | **100 %** | 220.4 MB | **100 %** |
| 3 | phone-sized player (`capLevelToPlayerSize`) | 360p only | 180 | 22 % | 62.2 MB | 11.5 % |
| **All** | | | **550** | **40.0 %** | **499.5 MB** | **44.8 %** |

Counted twice, in the browser (`X-Cache-Status` per response) and from the nginx log
(`make cache-stats`); the two agree. MinIO served 275.5 MB of the 499.5 MB. The cache held
265 MB afterwards for 10 clips. No query string appears anywhere in the nginx log.

What the numbers say:

- A repeat viewer **on the same rung** is served entirely from cache — the 100 % is what
  immutable versions buy.
- A viewer **on a different rung** shares almost nothing: only the playlists and the one 360p
  segment per clip that hls.js had used to measure bandwidth were already cached. ABR splits
  the cache by rung, so the real hit rate depends on how viewers' networks and screens spread
  across the ladder, not just on popularity.
- On localhost hls.js always settles on 720p. Real networks would spread viewers across rungs and
  make pass 3 the common case, so 100 % is the ceiling, not the expectation.
- A replay **in the same browser** never reaches nginx: segments carry
  `Cache-Control: immutable`, so the browser keeps them. The shared cache only pays off
  **across** viewers — which is exactly what a CDN is for.

**Why a CDN is needed at all.** Every viewer pulls about 22 MB per clip at 720p. Served from
storage, egress and storage request count grow with viewers × clips; behind a cache they grow
with clips × rungs, once per cache location. A single nginx on one machine does that for one
location. Viewers spread across a continent need it at many locations close to them — that is
the CDN. The design that makes it trivial is already in place: immutable versioned paths, a key
without the query string, and a mutable pointer that lives in the uncached API.

**Measurement pitfall I hit.** The first run reused the URLs of an aborted one. The browser had
cached them as immutable — with their `X-Cache-Status: MISS` header — so the "measurement" never
left the browser. The browser counted MISSes and the nginx log was empty. Cross-checking the
server side caught it; a fresh nonce per run fixed it.

**Run it locally.**

```bash
make up                              # starts nginx on 127.0.0.1:8081 with the rest
# .env: HLS_BASE_URL=http://localhost:8081, then restart `make api`
make fe                              # play an HLS clip: DevTools shows X-Cache-Status per request
make cache-stats SINCE=10m           # hit rate and byte hit rate from nginx's log
docker compose up -d --force-recreate nginx   # cold cache
```

**What I learned.** _(to write in my own words)_

**What I chose not to build — and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| A real CDN (CloudFront, Cloudflare): one local nginx | viewers are not on this machine — the streaming tier is never deployed (D11) |
| Cache purge or invalidation | any path under `hls/` ever holds bytes that change — today none can |
| The `slice` module or range-aware caching | clients request byte ranges of large files; HLS segments are ~1 MB and fetched whole |
| Cache warming or pre-fetching the next rung | first-viewer latency matters, e.g. a new clip expected to go viral |
| A persistent cache volume: recreating the container empties it | a restart must not cost a cold cache — on a laptop it is a feature for measuring |
| Access control: anyone on this machine can read `hls/` | that is M9 — the invite cookie, checked by nginx before the cache |

### M9 — segment access control and hardening, local (`m9-hardening`)

**What it does.** Since M5, anything on this machine could read every HLS playlist and segment
without a credential: hls.js resolves renditions and segments relative to the master, so those
requests carry no signature, and MinIO served `clips/*/hls/*` anonymously. Now a segment is
served only to a request that carries a valid invite cookie, the same `gs_invite` the API's gate
sets. Like M5–M8 it runs on localhost only and is shown by screencast. Phase B is never deployed.

**How it works.** The cookie check and the storage restriction depend on each other. Neither
works without the other.

- *nginx checks the cookie* (`deploy/local/nginx/invite.js`, njs). It applies the same rules as
  the API's `auth.VerifyInvite`: the value is `base64url(id|exp|HMAC-SHA256(INVITE_SECRET,
  id|exp))`. The MAC is compared in constant time, `exp` must be in the future, and `id` must
  still be in `INVITE_CODES`, so removing an id revokes every cookie it issued. Every failure is
  the same 403.
- *The check runs before the cache.* It is a `return 403` in nginx's rewrite phase, which runs
  before `proxy_cache` is consulted. A segment that is already cached is still refused to a
  request without the cookie.
- *MinIO serves `hls/` anonymously only to nginx.* The local bucket policy gains an
  `aws:SourceIp` condition naming nginx's fixed address on a fixed Docker subnet
  (`172.30.0.10`). A bare segment URL sent straight to MinIO is 403, so the cookie check cannot
  be bypassed by skipping nginx. Originals stay private and presigned, exactly as before.
- *The browser sends the cookie.* Cookies ignore ports, so `gs_invite`, set through the app on
  `localhost:5173`, is also sent to nginx on `localhost:8081`. The two are the same site, so
  `SameSite=Lax` is satisfied. They are different origins, though, so hls.js must send
  credentials (`xhrSetup` → `withCredentials`) and nginx answers with the exact origin plus
  `Access-Control-Allow-Credentials: true`, never `*`. Safari's native HLS sends same-site
  cookies without CORS. Without `withCredentials`, every HLS request is 403 and every clip falls
  back to MP4. The Go tests attach the cookie themselves and could not see this; a review caught
  it, and a browser check confirmed it.
- *Secrets stay out of config files and logs.* nginx reads `INVITE_SECRET` and `INVITE_CODES`
  from its environment (`env` in `nginx.conf`), never from a rendered config. The cookie is
  removed before the request reaches MinIO. The access log records `$uri` only: no query string,
  no cookie.
- *The cache key is still `$uri`.* A per-viewer token in the URL would split the cache into one
  copy per viewer. A cookie leaves the key alone, so a second viewer still gets a HIT.
- *`make verify-hls`* goes through nginx with a cookie it signs itself, the same path a player
  takes.

**What a URL alone grants, by milestone.**

| Milestone | URL | Grants |
|---|---|---|
| M1–M4.5 | presigned `original.mp4` | that one clip, for 15 minutes, with no gate and no login (accepted) |
| M5–M8 | `clips/<id>/hls/<version>/…` | readable forever by anything on this machine |
| M9 | the same, through nginx or straight to MinIO | nothing without a valid invite cookie: 403, even when cached |

**Accepted gap, verified.** MinIO takes the `aws:SourceIp` it checks from `X-Forwarded-For`,
`X-Real-IP` or `Forwarded` when a request carries one. So a process on this machine that sends
one naming nginx's address gets past the policy. I accepted it because MinIO is bound to
127.0.0.1, anything that can send that request can also read the MinIO data volume through
Docker, and a browser cannot set those headers on a media request. A test pins the behaviour,
so a MinIO upgrade that changes it will be noticed. A related fix: nginx now blanks those three
headers on its way to MinIO. Otherwise a client's own `X-Forwarded-For` would replace nginx's
address and turn a cold read into a 403.

**Request IDs, end to end.**

- Every API request now writes one log line: `request_id`, method, route pattern (never the
  query string), status, bytes and duration. The request ID matches the `X-Request-ID` response
  header and the `request_id` in the JSON body.
- `audit_log.request_id` and `actor_user_id` existed in the schema since M1, but nothing wrote
  them: 0 of 457 rows had either. They are now filled from the request context.
- A transcode can now be followed from the click to the worker: `X-Request-ID` → the
  `job_created` audit row → the job id → the worker's log lines, which carry `job`, `clip`,
  `attempt` and `output_version`. The queue entry stays a bare job id, because Redis holds no
  truth.

**Run it locally.**

```bash
make up                                  # nginx with the cookie check, MinIO restricted to nginx
K=clips/<id>/hls/<version>/master.m3u8   # any hls clip's live master
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8081/goalstream/$K   # 403
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9010/goalstream/$K   # 403 (MinIO direct)
# pass the gate in the app (make api, make fe): HLS clips play, every :8081 request is 200
make verify-hls                          # probes through nginx with a cookie it signs
```

**What I learned.** _(to write in my own words)_

**What I chose not to build, and when that choice would be wrong.**

| Not built | Would be wrong when |
|---|---|
| The production version: an owned domain (`app.`, `media.`), a private R2 bucket reachable only through Cloudflare Access with a service token added by the edge, a `Domain`-scoped invite cookie, and a deployed worker | real viewers need adaptive streaming in production. Today production serves MP4 only (D11) |
| A signed URL or token per segment | the cookie cannot reach the media host (another site with no shared domain). Until then a token only splits the cache |
| An API `auth_request` per segment | access depends on per-user state the cookie does not carry (subscriptions, per-clip rights). Today it would add one API call per segment |
| Closing the forwarded-header bypass of the MinIO policy | MinIO is reachable from anything other than this machine, or an untrusted process runs here |
| A cookie check on presigned originals | the 15-minute presign gap stops being acceptable (plan §17) |
| DRM, or per-viewer watermarking | the content's owner requires it. The NDA requires access control, not copy protection |
| Correlating logs across services (a trace ID through Redis to the worker) | jobs fan out across services, or a job's log lines cannot be found from its id. Today the audit row joins them |
