#!/usr/bin/env bash
# Offline persistent-storage smoke tests, also run in the standard-user suite.
set -u
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.." || exit 1
root=$(cygpath -w "$PWD")
quasidock=${1:-$root/out/bin/quasidock.exe}
go run "$root/scripts/build.go" cli-fixture || exit 1
temp_parent=$(cd "${TMPDIR:-/tmp}" && pwd -P) || exit 1
temp_u=$(mktemp -d "$temp_parent/quasidock-mounts.XXXXXXXX") || exit 1
tmp=$(cygpath -w "$temp_u")
export QUASIDOCK_HOME="$tmp\\store"
bind="$tmp\\host files"
mkdir -p "$bind"
cleanup() {
    "$quasidock" rm -f first second >/dev/null 2>&1 || true
    "$quasidock" container prune >/dev/null 2>&1 || true
    "$quasidock" volume prune --all >/dev/null 2>&1 || true
    "$quasidock" rmi test.mounts.fixture >/dev/null 2>&1 || true
    case "$temp_u" in "$temp_parent"/quasidock-mounts.*) rm -rf -- "$temp_u" ;; esac
}
trap cleanup EXIT
"$quasidock" build "$root/tests/mounts/app.json" >"$temp_u/build.log" 2>&1 || { echo 'building mount fixture failed'; tail -12 "$temp_u/build.log"; exit 1; }
app=test.mounts.fixture
failures=0
check() {
    local label=$1 out
    shift
    if out=$("$@" 2>&1); then printf 'ok    %s\n' "$label"
    else printf 'FAIL  %s\n      %s\n' "$label" "$(tail -1 <<<"$out" | cut -c1-160)"; failures=$((failures+1)); fi
}
root_persists() {
    local sb=$1 got
    "$quasidock" run --rm --sandbox="$sb" -v "saved-$sb:\${data}" "$app" fixture.exe write "saved-$sb" || return
    got=$("$quasidock" run --rm --sandbox="$sb" -v "saved-$sb:\${data}" "$app" fixture.exe read | tr -d '\r') || return
    [ "$got" = "saved-$sb" ]
}
bind_persists() {
    local sb=$1
    "$quasidock" run --rm --sandbox="$sb" --mount "type=bind,source=$bind,target=\${data}/workspace" "$app" fixture.exe write-path '$HOME\..\workspace\sentinel' host-data || return
    [ "$(cat "$bind\\sentinel")" = host-data ]
}
for sb in soft low appcontainer; do
    check "named root volume persists with --rm under $sb" root_persists "$sb"
    check "host subpath mount writes through under $sb" bind_persists "$sb"
done
volume_metadata() {
    [ "$("$quasidock" volume create saved-soft | tr -d '\r')" = saved-soft ] &&
    [ "$("$quasidock" volume inspect -f '{{.Driver}}' SAVED-SOFT | tr -d '\r')" = local ]
}
check 'volume create is idempotent and inspection is case-insensitive' volume_metadata
detached_sharing() {
    local i
    "$quasidock" run -d --name first --sandbox=appcontainer -v "${bind}:\${data}" "$app" fixture.exe hold-path '$HOME\first' >/dev/null || return
    "$quasidock" run -d --name second --sandbox=low -v "${bind}:\${data}" "$app" fixture.exe hold-path '$HOME\second' >/dev/null || return
    for i in $(seq 1 100); do [ -f "$bind\\home\\first" ] && [ -f "$bind\\home\\second" ] && break; sleep .05; done
    [ -f "$bind\\home\\first" ] && [ -f "$bind\\home\\second" ] || return 1
    "$quasidock" rm -f first >/dev/null || return
    sleep .3
    [ "$("$quasidock" inspect -f '{{.State.Running}}' second | tr -d '\r')" = true ] || return 1
    "$quasidock" rm -f second >/dev/null || return
    [ -f "$bind\\home\\first" ] && [ -f "$bind\\home\\second" ]
}
check 'detached consumers share data and survive peer removal' detached_sharing
container_cleanup() {
    "$quasidock" run --name retained -v 'saved-soft:${data}' "$app" fixture.exe read >/dev/null || return
    if "$quasidock" volume rm saved-soft >/dev/null 2>&1; then return 1; fi
    "$quasidock" container prune >/dev/null || return
    "$quasidock" prune --data >/dev/null || return
    "$quasidock" volume inspect saved-soft >/dev/null && [ -f "$bind\\sentinel" ]
}
check 'retained containers pin volumes and ordinary pruning preserves data' container_cleanup
dry_run() {
    "$quasidock" volume prune --all --dry-run >/dev/null && "$quasidock" volume inspect saved-soft >/dev/null
}
check 'volume pruning dry run preserves volumes' dry_run
reject_ro() { ! "$quasidock" run --rm -v 'saved-soft:${data}:ro' "$app" >/dev/null 2>&1; }
check 'unsupported read-only mounts fail' reject_ro
explicit_prune() {
    "$quasidock" volume prune --all >/dev/null || return
    [ -z "$("$quasidock" volume ls -q | tr -d '\r')" ] && [ -f "$bind\\sentinel" ]
}
check 'explicit volume pruning leaves host bind contents intact' explicit_prune
echo "$failures check(s) failed."
exit $((failures>0))
