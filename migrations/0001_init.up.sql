-- M1 tables (docs/SCHEMA.md). clips gains hls_version + thumbnail_key in M5 (expand/contract);
-- sessions/feedback/seen_clips arrive in M2, auth + taste tables in M4.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE matches (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  soccernet_path TEXT UNIQUE NOT NULL,
  home_team      TEXT NOT NULL,
  away_team      TEXT NOT NULL,
  competition    TEXT NOT NULL,
  season         TEXT NOT NULL,
  match_date     DATE,
  created_at     TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE vocab (
  kind       TEXT NOT NULL,
  value      TEXT NOT NULL,
  slot       INT  NOT NULL,
  created_at TIMESTAMPTZ DEFAULT now(),
  PRIMARY KEY (kind, value),
  UNIQUE (kind, slot),
  CONSTRAINT vocab_kind_chk CHECK (kind IN ('team','competition','season','tag')),
  CONSTRAINT vocab_slot_chk CHECK (slot >= 0 AND slot < CASE kind
                                     WHEN 'team'        THEN 64
                                     WHEN 'competition' THEN 8
                                     WHEN 'season'      THEN 8
                                     WHEN 'tag'         THEN 32 END)
);

-- Frozen tag vocabulary v1 (docs/DATASET.md § Tags), slots 0..12 in this order.
INSERT INTO vocab (kind, value, slot) VALUES
  ('tag','header',0), ('tag','free_kick',1), ('tag','penalty',2), ('tag','long_range',3),
  ('tag','inside_box',4), ('tag','solo_run',5), ('tag','counter',6), ('tag','team_move',7),
  ('tag','deflection',8), ('tag','volley',9), ('tag','late_goal',10), ('tag','equaliser',11),
  ('tag','winner',12);

CREATE TABLE clips (
  id               UUID PRIMARY KEY,     -- generated in Go: the object is uploaded before the row
  match_id         UUID NOT NULL REFERENCES matches(id),
  half             INT  NOT NULL,
  position_ms      INT  NOT NULL,
  event_type       TEXT NOT NULL,
  title            TEXT NOT NULL,
  scoring_team     TEXT,
  is_home          BOOLEAN,
  match_minute     INT  NOT NULL,
  tags             TEXT[] NOT NULL DEFAULT '{}',
  s3_prefix        TEXT NOT NULL,
  source_type      TEXT NOT NULL DEFAULT 'mp4',
  width            INT  NOT NULL,
  height           INT  NOT NULL,
  source_bitrate   INT  NOT NULL,
  duration_ms      INT  NOT NULL,
  embedding        vector(128) NOT NULL,
  created_at       TIMESTAMPTZ DEFAULT now(),
  CONSTRAINT clips_source_type_chk CHECK (source_type IN ('mp4','hls')),
  CONSTRAINT clips_half_chk        CHECK (half IN (1,2)),
  CONSTRAINT clips_position_chk    CHECK (position_ms >= 0),
  CONSTRAINT clips_dedup_uniq      UNIQUE (match_id, half, position_ms, event_type)
);

CREATE INDEX clips_source_type_idx ON clips (source_type);
CREATE INDEX clips_created_idx     ON clips (created_at DESC, id DESC);

CREATE TABLE users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email         TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  display_name  TEXT,
  is_admin      BOOLEAN NOT NULL DEFAULT false,
  created_at    TIMESTAMPTZ DEFAULT now(),
  CONSTRAINT users_email_lower_chk CHECK (email = lower(btrim(email)))
);

CREATE TABLE audit_log (
  id            BIGSERIAL PRIMARY KEY,
  entity_type   TEXT NOT NULL,
  entity_id     UUID NOT NULL,
  action        TEXT NOT NULL,
  actor_type    TEXT NOT NULL,
  actor_user_id UUID,
  before_state  JSONB,
  after_state   JSONB,
  metadata      JSONB,
  request_id    TEXT,
  created_at    TIMESTAMPTZ DEFAULT now(),
  CONSTRAINT audit_entity_chk CHECK (entity_type IN ('clip','match','job','user')),
  CONSTRAINT audit_actor_chk  CHECK (actor_type  IN ('user','admin','worker','system'))
);

CREATE INDEX audit_entity_idx  ON audit_log (entity_type, entity_id, created_at DESC);
CREATE INDEX audit_request_idx ON audit_log (request_id);
