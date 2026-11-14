#!/usr/bin/env bash
# Console events end to end: Ctrl+C, Ctrl+Break and closing the console reach
# an app run through quasidock, as they reach it run alone, and the app's process
# tree ends with the run.
#
#   tests/ctrlc.sh [path\to\quasidock.exe]
#
# Needs Git Bash and network access (for the Node download, unless the store
# in %QUASIDOCK_HOME% or SEED_DOWNLOADS has it). Builds the test driver
# (tests/cmd/ctrlc), which runs each app in a pseudoconsole, as a terminal
# does, so no window opens. Uses a temporary store unless QUASIDOCK_HOME is set.

set -u
export MSYS_NO_PATHCONV=1
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"

cd "$(dirname "$0")/.." || exit 1
root=$(cygpath -w "$(pwd)")
quasidock=${1:-$root/out/bin/quasidock.exe}
go run "$(cygpath -w "$root/scripts/build.go")" ctrlc-driver || { echo "building the test driver failed"; exit 1; }

own_store=false
if [ -z "${QUASIDOCK_HOME:-}" ]; then
    QUASIDOCK_HOME=$(cygpath -w "$(mktemp -d)")
    own_store=true
fi
export QUASIDOCK_HOME
work=$(cygpath -w "$(mktemp -d)")
# SEED_DOWNLOADS: another store's cache\downloads, whose files are linked, or
# copied, into this one's, so as not to download them again.
if [ -n "${SEED_DOWNLOADS:-}" ]; then
    mkdir -p "$QUASIDOCK_HOME\\cache\\downloads"
    cp -l "$(cygpath -u "$SEED_DOWNLOADS")"/* "$(cygpath -u "$QUASIDOCK_HOME")/cache/downloads/" 2>/dev/null ||
        cp -n "$(cygpath -u "$SEED_DOWNLOADS")"/* "$(cygpath -u "$QUASIDOCK_HOME")/cache/downloads/"
fi

"$quasidock" build "$root/recipes/node.json" >/dev/null 2>&1 || { echo "building node failed"; exit 1; }
"$root/out/test/quasidock-ctrlc.exe" "$quasidock" "$QUASIDOCK_HOME" "$work"
status=$?

if $own_store; then
    qd_remove_app "$quasidock" org.nodejs.node >/dev/null 2>&1
    "$quasidock" prune --downloads >/dev/null 2>&1
    rm -rf "$QUASIDOCK_HOME"
fi
rm -rf "$work"
exit $status
