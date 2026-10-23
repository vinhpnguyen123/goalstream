-- M4: refresh tokens (rotating, hashed, family-tracked) and the recommender's online state.
-- docs/SCHEMA.md tables 5 and 9.

CREATE TABLE refresh_tokens (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash     TEXT NOT NULL UNIQUE,
  family_id      UUID NOT NULL,
  expires_at     TIMESTAMPTZ NOT NULL,
  revoked_at     TIMESTAMPTZ,
  revoked_reason TEXT,
  created_at     TIMESTAMPTZ DEFAULT now(),
  CONSTRAINT refresh_revoked_reason_chk CHECK (revoked_reason IN ('rotated','reuse','logout')),
  CONSTRAINT refresh_revoked_pair_chk   CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL))
);

CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

CREATE TABLE user_taste (
  user_id           UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  positive_centroid vector(128),
  negative_centroid vector(128),
  liked_count       INT NOT NULL DEFAULT 0,
  disliked_count    INT NOT NULL DEFAULT 0,
  updated_at        TIMESTAMPTZ DEFAULT now(),
  CONSTRAINT taste_pos_chk CHECK ((positive_centroid IS NULL) = (liked_count = 0)),
  CONSTRAINT taste_neg_chk CHECK ((negative_centroid IS NULL) = (disliked_count = 0))
);
