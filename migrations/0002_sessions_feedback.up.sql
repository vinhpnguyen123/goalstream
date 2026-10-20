-- M2 tables (docs/SCHEMA.md 6–8). user_taste arrives in M4; until then feedback writes only
-- the event and seen_clips.
CREATE TABLE sessions (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  started_at TIMESTAMPTZ DEFAULT now(),
  ended_at   TIMESTAMPTZ               -- set by POST /api/v1/sessions/{id}/end (guarded: IS NULL)
);

CREATE INDEX sessions_user_idx ON sessions (user_id);

-- APPEND-ONLY rating log: the current rating of a clip is the latest row for (user_id, clip_id).
CREATE TABLE feedback_events (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id   UUID REFERENCES sessions(id) ON DELETE CASCADE,  -- NULL = re-rated outside a session
  user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  clip_id      UUID NOT NULL REFERENCES clips(id),
  response     TEXT NOT NULL,
  replay_count INT  NOT NULL DEFAULT 0,
  watched_full BOOLEAN,                  -- NULL on a re-rate (nothing was watched)
  created_at   TIMESTAMPTZ DEFAULT now(),
  CONSTRAINT feedback_response_chk CHECK (response IN ('loved','okay','not_for_me')),
  CONSTRAINT feedback_replay_chk   CHECK (replay_count >= 0),
  CONSTRAINT feedback_session_chk  CHECK (session_id IS NOT NULL OR watched_full IS NULL)
);

CREATE INDEX feedback_latest_idx ON feedback_events (user_id, clip_id, created_at DESC, id DESC);

-- Written on FEEDBACK, never on presign.
CREATE TABLE seen_clips (
  user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  clip_id       UUID NOT NULL REFERENCES clips(id),
  first_seen_at TIMESTAMPTZ DEFAULT now(),
  PRIMARY KEY (user_id, clip_id)
);
