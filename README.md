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
