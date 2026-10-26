-- M5: HLS delivery. docs/SCHEMA.md tables 3 (hls columns), 10 and 11.
-- Ships to production with every deploy (expand); production runs no worker, so it stays unused there.

ALTER TABLE clips
  ADD COLUMN hls_version   UUID,  -- output_version of the LIVE hls prefix
  ADD COLUMN thumbnail_key TEXT,  -- key under the live version prefix
  -- an hls clip MUST point at a version; an mp4 clip may keep one (rollback target)
  ADD CONSTRAINT clips_hls_version_chk CHECK (source_type <> 'hls' OR hls_version IS NOT NULL);

-- What qualities exist per version; the API never lists the bucket.
CREATE TABLE renditions (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  clip_id       UUID NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
  hls_version   UUID NOT NULL,           -- = transcode_jobs.output_version of the producing attempt
  height        INT  NOT NULL,
  width         INT  NOT NULL,           -- read back by ffprobe from the rendition
  video_bitrate INT  NOT NULL,           -- target (average)
  max_rate      INT  NOT NULL,           -- encoder -maxrate
  audio_bitrate INT  NOT NULL,
  bandwidth     INT  NOT NULL,           -- PEAK, exactly as written into master.m3u8
  segment_count INT  NOT NULL,
  created_at    TIMESTAMPTZ DEFAULT now(),
  UNIQUE (clip_id, hls_version, height)
);

CREATE TABLE transcode_jobs (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  clip_id        UUID NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
  status         TEXT NOT NULL DEFAULT 'queued',
  attempts       INT  NOT NULL DEFAULT 0,
  output_version UUID,                   -- minted FRESH at every claim: this attempt's prefix and fencing token
  failure_reason TEXT,                   -- last attempt's reason (IMPLEMENTATION_PLAN §6 vocabulary)
  error          TEXT,                   -- last 8 KiB of ffmpeg stderr
  started_at     TIMESTAMPTZ,            -- set on claim; the stuck-job clock
  created_at     TIMESTAMPTZ DEFAULT now(),
  updated_at     TIMESTAMPTZ DEFAULT now(),   -- set on every transition; the requeue clock
  CONSTRAINT jobs_status_chk
    CHECK (status IN ('queued','processing','retryable','done','failed')),
  CONSTRAINT jobs_processing_chk
    CHECK (status <> 'processing' OR (output_version IS NOT NULL AND started_at IS NOT NULL))
);

CREATE INDEX jobs_requeue_idx ON transcode_jobs (updated_at) WHERE status IN ('queued','retryable');
CREATE INDEX jobs_stuck_idx   ON transcode_jobs (started_at) WHERE status = 'processing';
CREATE INDEX jobs_clip_idx    ON transcode_jobs (clip_id, created_at DESC);

-- One active job per clip: two jobs for one clip would race the hls_version pointer.
CREATE UNIQUE INDEX jobs_one_active_per_clip ON transcode_jobs (clip_id)
  WHERE status IN ('queued','processing','retryable');
