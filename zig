#!/usr/bin/env bash
root=$(cd "$(dirname "$0")" && pwd)
if [ "${1:-}" = "build" ]; then
    shift
    exec go run "$(cygpath -w "$root/scripts/build.go")" "$@"
elif [ "${1:-}" = "version" ]; then
    exec go version
fi
echo "Unknown command ${1:-}" >&2
exit 1
