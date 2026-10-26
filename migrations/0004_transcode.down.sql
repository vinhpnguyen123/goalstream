DROP TABLE IF EXISTS transcode_jobs;
DROP TABLE IF EXISTS renditions;
-- an hls clip cannot survive losing its version column: fall back to the original first (L3)
UPDATE clips SET source_type = 'mp4' WHERE source_type = 'hls';
ALTER TABLE clips
  DROP CONSTRAINT IF EXISTS clips_hls_version_chk,
  DROP COLUMN IF EXISTS thumbnail_key,
  DROP COLUMN IF EXISTS hls_version;
