#!/bin/sh
# One-shot init for local MinIO: create the app and test buckets, private except for one
# LOCAL-ONLY policy (IMPLEMENTATION_PLAN §8): anonymous GET on clips/*/hls/*. Only the HLS master
# is presigned; the player resolves renditions and segments relative to it and the signature does
# not carry over, so those requests are unsigned. Originals stay private (presigned only), MinIO
# binds 127.0.0.1, and this policy is NEVER applied to R2 — Phase B is never deployed.
set -eu
# MinIO has no healthcheck (no curl in its image), and `mc alias set` already contacts the
# server — so the alias itself is the readiness probe, retried for up to ~60s.
i=0
until mc alias set local http://minio:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -ge 60 ] && { echo "minio not ready after 60s" >&2; exit 1; }; sleep 1
done
mc mb --ignore-existing "local/$S3_BUCKET" "local/$S3_TEST_BUCKET"
for b in "$S3_BUCKET" "$S3_TEST_BUCKET"; do
  sed "s/BUCKET/$b/" /hls-read-policy.json > /tmp/policy.json
  mc anonymous set-json /tmp/policy.json "local/$b"
done
echo "buckets ready: $S3_BUCKET $S3_TEST_BUCKET"
