#!/bin/sh
# Uses installed Go/Redis, or the project-local tools prepared in this workspace.
set -eu
cd "$(dirname "$0")/.."
mkdir -p .tools
redis_bin=$(command -v redis-server || true)
if [ -z "$redis_bin" ]; then redis_bin="$PWD/.tools/redis-7.2.7/src/redis-server"; fi
if [ ! -x "$redis_bin" ]; then
  echo 'Redis is required. Install Redis, or use docker compose (see README).' >&2
  exit 1
fi
if [ ! -f .tools/demo-secret ]; then
  (umask 077; openssl rand -hex 32 > .tools/demo-secret)
fi
export TOKEN_SECRET="${TOKEN_SECRET:-$(cat .tools/demo-secret)}"
if [ ! -f .tools/demo-admin-token ]; then
  (umask 077; openssl rand -hex 8 > .tools/demo-admin-token)
fi
export ADMIN_TOKEN="${ADMIN_TOKEN:-$(cat .tools/demo-admin-token)}"
export ACTIVE_LIMIT="${ACTIVE_LIMIT:-2}"
# Use a separate event for each demo capacity, so config mismatch fails safely.
export EVENT_IDS="${EVENT_IDS:-demo-$ACTIVE_LIMIT}"
export REDIS_URL="${REDIS_URL:-redis://127.0.0.1:16380/0}"
redis_pid=
cleanup() { if [ -n "$redis_pid" ]; then kill "$redis_pid" 2>/dev/null || true; wait "$redis_pid" 2>/dev/null || true; fi; }
trap cleanup EXIT
trap 'exit 130' INT TERM
if [ "$REDIS_URL" = redis://127.0.0.1:16380/0 ]; then
  mkdir -p .tools/demo-redis
  "$redis_bin" --bind 127.0.0.1 --port 16380 --dir "$PWD/.tools/demo-redis" --appendonly yes --save '' > .tools/demo-redis.log 2>&1 &
  redis_pid=$!
  sleep 1
  if ! kill -0 "$redis_pid" 2>/dev/null; then echo 'Could not start Redis. See .tools/demo-redis.log' >&2; exit 1; fi
fi
site="http://${HTTP_ADDR:-localhost:8080}"
echo "User page:  $site (event ${EVENT_IDS%%,*}, capacity $ACTIVE_LIMIT)"
echo "Admin page: $site/admin (password: $ADMIN_TOKEN)"
scripts/go-local.sh run ./cmd/server
