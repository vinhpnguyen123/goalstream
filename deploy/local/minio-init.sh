#!/bin/sh
# One-shot init for local MinIO: create the app and test buckets. Both stay PRIVATE —
# originals are only ever reached through presigned URLs. (The M5 anonymous-read policy on
# clips/*/hls/* is added in M5, and is never applied to R2.)
set -eu
# MinIO has no healthcheck (no curl in its image), and `mc alias set` already contacts the
# server — so the alias itself is the readiness probe, retried for up to ~60s.
i=0
until mc alias set local http://minio:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -ge 60 ] && { echo "minio not ready after 60s" >&2; exit 1; }; sleep 1
done
mc mb --ignore-existing "local/$S3_BUCKET" "local/$S3_TEST_BUCKET"
echo "buckets ready: $S3_BUCKET $S3_TEST_BUCKET"
