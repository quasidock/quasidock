#!/usr/bin/env bash
# End-to-end checks for `quasidock build` and `quasidock prune`: replaced and
# removed versions are cleaned up, and nothing a running app uses is deleted.
#
#   tests/store.sh [path\to\quasidock.exe]
#
# Needs Git Bash and network access (for the busybox download, unless
# SEED_DOWNLOADS has it). Always uses a temporary store, since it deletes
# things, and removes it afterwards.

set -u
export MSYS_NO_PATHCONV=1

root=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
quasidock=${1:-$root/out/bin/quasidock.exe}
QUASIDOCK_HOME=$(cygpath -w "$(mktemp -d)")
export QUASIDOCK_HOME
work=$(cygpath -w "$(mktemp -d)")
# SEED_DOWNLOADS: another store's cache\downloads, whose files are linked, or
# copied, into this one's, so as not to download them again.
if [ -n "${SEED_DOWNLOADS:-}" ]; then
    mkdir -p "$QUASIDOCK_HOME\\cache\\downloads"
    cp -l "$(cygpath -u "$SEED_DOWNLOADS")"/* "$(cygpath -u "$QUASIDOCK_HOME")/cache/downloads/" 2>/dev/null ||
        cp -n "$(cygpath -u "$SEED_DOWNLOADS")"/* "$(cygpath -u "$QUASIDOCK_HOME")/cache/downloads/"
fi

BB=net.frippery.busybox

failures=0
check() {
    local label=$1
    shift
    local out
    if out=$("$@" 2>&1); then
        printf 'ok    %s\n' "$label"
    else
        printf 'FAIL  %s\n      %s\n' "$label" "$(grep -v '^\s*$' <<<"$out" | tail -1 | cut -c1-110)"
        failures=$((failures + 1))
    fi
}

z() { "$quasidock" "$@"; }
deployments() { find "$QUASIDOCK_HOME/deploy" -mindepth 1 -maxdepth 1 -type d | wc -l; }
blobs() { find "$QUASIDOCK_HOME/blobs/sha256" -type f | wc -l; }
expect_count() { local got; got=$("$1"); [ "$got" -eq "$2" ] || { echo "$1: got $got, want $2"; return 1; }; }

# A copy of the busybox recipe whose version the checks change. Each version
# also has a marker file saying which it is, so its files differ: deployments
# are per layer, and versions with the same files would share one.
recipe="$work\\busybox.json"
sed 's/"dest": "busybox.exe"/"dest": "busybox.exe" }, { "path": "marker.txt"/' "$root/recipes/busybox.json" >"$recipe"
echo original >"$work\\marker.txt"
# (A POSIX path: sed -i can't move its temporary file across drives.)
set_version() {
    sed -i "s/\"version\": \"[^\"]*\"/\"version\": \"$1\"/" "$(cygpath -u "$recipe")"
    echo "$1" >"$work\\marker.txt"
}
installed_version() { z list | awk -v id=$BB '$1 == id { print $2 }'; }
z build "$recipe" >/dev/null 2>&1 || { echo "building busybox failed"; exit 1; }

# --- build replacement -------------------------------------------------------

changed_recipe() {
    set_version test-2
    z build "$recipe" >/dev/null 2>&1 && [ "$(installed_version)" = test-2 ] && expect_count deployments 1
}
check "build: a changed recipe replaces the old version" changed_recipe

# --- in-use protection ---------------------------------------------------------

# Starts `busybox sleep` on the installed version, in the background.
z run $BB sleep 6 &
running=$!
sleep 1
build_while_running() {
    set_version test-3
    z build "$recipe" 2>&1 | grep -q 'still running' && [ "$(installed_version)" = test-3 ] && expect_count deployments 2
}
check "build: a running version's files are kept" build_while_running
prune_while_running() { z prune 2>&1 | grep -q 'kept 1 unused deployment' && expect_count deployments 2; }
check "prune: keeps a deployment in use" prune_while_running
# `check` runs commands in a subshell, which can't wait for this shell's jobs.
wait $running
status=$?
check "the running app finishes normally" test $status -eq 0

dry_run() { z prune --dry-run 2>&1 | grep -q '^would remove 1 deployment' && expect_count deployments 2 && expect_count blobs 6; }
check "prune --dry-run deletes nothing" dry_run
prune_after_exit() { z prune >/dev/null 2>&1 && expect_count deployments 1 && expect_count blobs 3; }
check "prune: removes it once unused, and unused blobs" prune_after_exit

# --- tmp\ ----------------------------------------------------------------------

z run --ephemeral $BB sh -c 'echo x > "$APPDATA/f" && sleep 5' &
ephemeral=$!
sleep 1
mkdir "$QUASIDOCK_HOME\\tmp\\stale-dir" && echo x >"$QUASIDOCK_HOME\\tmp\\stale-dir\\f" && echo x >"$QUASIDOCK_HOME\\tmp\\stale-file"
run_dirs() { find "$QUASIDOCK_HOME/tmp" -mindepth 1 -maxdepth 1 -name 'run-*' | wc -l; }
prune_tmp() {
    z prune 2>&1 | grep -q 'kept the data of 1 running --ephemeral app' &&
        [ ! -e "$QUASIDOCK_HOME\\tmp\\stale-dir" ] && [ ! -e "$QUASIDOCK_HOME\\tmp\\stale-file" ] &&
        expect_count run_dirs 2 # The directory and its lock.
}
check "prune: clears tmp\\ but keeps a running --ephemeral app's data" prune_tmp
wait $ephemeral
status=$?
ephemeral_done() { [ $status -eq 0 ] && expect_count run_dirs 0; }
check "the --ephemeral run cleans up after itself" ephemeral_done

# --- rm, orphaned data and downloads ---------------------------------------------

z run $BB sleep 5 &
running=$!
sleep 1
rm_while_running() { z rm $BB 2>&1 | grep -q 'still in use' && expect_count deployments 1; }
check "rm: keeps the files of a running app" rm_while_running
wait $running
orphaned_data() { z prune 2>&1 | grep -q "kept data of apps that aren't installed: $BB" && expect_count deployments 0; }
check "prune: removes them afterwards, and lists orphaned data" orphaned_data
check "prune --data deletes it" sh -c "\"\$0\" prune --data >/dev/null 2>&1 && [ ! -e '$QUASIDOCK_HOME\\data\\$BB' ]" "$quasidock"
downloads() { find "$QUASIDOCK_HOME/cache/downloads" -type f | wc -l; }
prune_downloads() { z prune --downloads >/dev/null 2>&1 && expect_count downloads 0; }
check "prune --downloads clears the download cache" prune_downloads
# Files of Windows' roots that earlier runs had (internal/roots).
stale_roots() { : >"$QUASIDOCK_HOME\\certs\\roots-0000000000000000.pem" && z prune >/dev/null 2>&1 && [ ! -e "$QUASIDOCK_HOME\\certs\\roots-0000000000000000.pem" ]; }
check "prune deletes files of Windows' roots that aren't today's" stale_roots

# --- the store lock --------------------------------------------------------------

# Holds the store lock for 3 seconds.
powershell -NoProfile -Command "\$f = [IO.File]::Open('$QUASIDOCK_HOME\\lock', 'OpenOrCreate', 'ReadWrite', 'ReadWrite'); \$f.Lock(0, 1); 'locked'; Start-Sleep 3; \$f.Close()" >"$work\\held" &
holder=$!
until grep -q locked "$work\\held" 2>/dev/null; do sleep 0.2; done
waits_for_lock() {
    local start=$SECONDS out
    out=$(z prune 2>&1) && [ $((SECONDS - start)) -ge 1 ]
}
check "prune waits for the store lock" waits_for_lock
wait $holder

z prune --downloads --data >/dev/null 2>&1
rm -rf "$QUASIDOCK_HOME" "$work"
echo
echo "$failures check(s) failed."
exit $((failures > 0))
