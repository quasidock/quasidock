#!/usr/bin/env bash
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
exec go run "$(cygpath -w "$root/scripts/build.go")" "$@"
