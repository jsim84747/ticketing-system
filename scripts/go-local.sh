#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
if command -v go >/dev/null 2>&1; then
  exec go "$@"
fi
export GOPATH="$PWD/.tools/gopath" GOCACHE="$PWD/.tools/gocache"
exec .tools/go/bin/go "$@"
