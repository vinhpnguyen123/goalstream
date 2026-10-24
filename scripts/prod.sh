#!/bin/sh
# Runs a command with ONLY .env.prod in its environment. `make` exports every variable from
# .env, so without `env -i` a variable missing from .env.prod would silently take its local
# value (APP_ENV=dev, the local bucket) — this way it fails config instead.
set -eu
[ -f .env.prod ] || { echo "prod.sh: .env.prod not found" >&2; exit 1; }
exec env -i PATH="$PATH" HOME="$HOME" sh -c 'set -a; . ./.env.prod; set +a; exec "$@"' prod "$@"
