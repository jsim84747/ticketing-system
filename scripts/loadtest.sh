#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
exec scripts/go-local.sh run ./cmd/loadtest "$@"
