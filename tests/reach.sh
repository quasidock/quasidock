#!/usr/bin/env bash
# What a run may reach of the rest of the machine, end to end, in a store of
# its own, with small test apps:
#
# - --share=host: quasidock-host, which runs a program in the user's real
#   environment, with the run's rights;
# - an image saying it can't run under a sandbox, on the console and in a saved
#   override.
#
#   bash tests/reach.sh [path\to\quasidock.exe]
#
# Needs Git Bash and PowerShell.

set -u
export MSYS_NO_PATHCONV=1

root=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
quasidock=${1:-$root/zig-out/bin/quasidock.exe}
work=$(cygpath -w "$(mktemp -d)")
QUASIDOCK_HOME="$work\\store"
export QUASIDOCK_HOME
export QUASIDOCK_GRANTS_KEY="Software\\QuasidockTest\\reach-$$-grants"
scratch="$work\\cwd"
mkdir -p "$scratch"

failures=0
check() {
    local label=$1
    shift
    local out
    if out=$("$@" 2>&1); then
        printf 'ok    %s\n' "$label"
    else
        printf 'FAIL  %s\n      %s\n' "$label" "$(tail -1 <<<"$out" | cut -c1-150)"
        failures=$((failures + 1))
    fi
}
has() { grep -qF -- "$2" <<<"$1" || { echo "no \"$2\" in: $1"; return 1; }; }
hasnt() { ! grep -qF -- "$2" <<<"$1" || { echo "\"$2\" in: $1"; return 1; }; }

(cd "$root" && go run ./scripts/build.go argv-echo) || { echo "building the test programs failed"; exit 1; }
cd "$scratch" || exit 1
for app in outer; do
    "$quasidock" build "$root\\tests\\reach\\$app.json" >/dev/null 2>&1 || { echo "building $app failed"; exit 1; }
done

# in_run <run options...> -- <cmd.exe command>: runs it in test.reach.outer.
in_run() {
    local opts=()
    while [ "$1" != -- ]; do opts+=("$1"); shift; done
    shift
    printf '@%s\r\n' "$1" >"$work\\inner.cmd"
    "$quasidock" run "${opts[@]}" --command=cmd test.reach.outer /c "$work\\inner.cmd" 2>&1
}

# --- --share=host ---
real_home=$(powershell -NoProfile -Command "[Environment]::GetFolderPath('UserProfile')" | tr -d '\r')
host_env() {
    local out sb
    for sb in soft low; do
        out=$(in_run --sandbox=$sb --share=host -- 'quasidock-host cmd /c set USERPROFILE')
        has "$out" "USERPROFILE=$real_home" || return 1
        hasnt "$out" "$QUASIDOCK_HOME" || return 1
    done
}
check "host: the program gets your real environment, under soft and low" host_env
host_rights() {
    local out
    out=$(in_run --sandbox=low --share=host -- "quasidock-host \"$root\\zig-out\\test\\quasidock-hostprobe.exe\"")
    has "$out" "# integrity low" || return 1
    out=$(in_run --share=host -- "quasidock-host \"$root\\zig-out\\test\\quasidock-hostprobe.exe\"")
    hasnt "$out" "# integrity low" || return 1
}
check "host: it keeps the run's rights: at low in a low run" host_rights
host_exit() {
    local code
    printf '@quasidock-host cmd /c exit 9\r\n@exit /b %%errorlevel%%\r\n' >"$work\\inner.cmd"
    "$quasidock" run --share=host --command=cmd test.reach.outer /c "$work\\inner.cmd" >/dev/null 2>&1
    code=$?
    [ "$code" = 9 ] || { echo "exit code $code"; return 1; }
}
check "host: its exit code comes back" host_exit
host_missing() {
    local out code
    printf '@quasidock-host quasidock-no-such-program\r\n@exit /b %%errorlevel%%\r\n' >"$work\\inner.cmd"
    out=$("$quasidock" run --share=host --command=cmd test.reach.outer /c "$work\\inner.cmd" 2>&1)
    code=$?
    [ "$code" = 127 ] || { echo "exit code $code"; return 1; }
    has "$out" "isn't on your PATH" || return 1
}
check "host: a program it can't find is an error" host_missing
host_batch() {
    local out
    out=$(in_run --share=host -- "quasidock-host \"$root\\tests\\reach\\echo.cmd\" \"a b\"")
    has "$out" 'batch got "a b"' || return 1
}
check "host: it runs a batch file" host_batch
host_off() {
    local out
    out=$(in_run -- 'where quasidock-host')
    hasnt "$out" "quasidock-host.exe" || return 1
    out=$(in_run --sandbox=appcontainer --share=host -- 'where quasidock-host')
    hasnt "$out" "quasidock-host.exe" || return 1
    has "$out" "doesn't get quasidock-host" || return 1
}
check "host: not without it, nor in an AppContainer, which says so" host_off

# --- an image that can't run under a sandbox ---
warns() {
    local out
    out=$("$quasidock" run --sandbox=appcontainer --command=cmd test.reach.outer /c ver 2>&1)
    has "$out" "warning: test.reach.outer can't run under the appcontainer sandbox: the test says it can't" || return 1
    has "$out" "--sandbox=soft" || return 1
    out=$("$quasidock" run --command=cmd test.reach.outer /c ver 2>&1)
    hasnt "$out" "can't run under" || return 1
    out=$("$quasidock" run --sandbox=low --command=cmd test.reach.outer /c ver 2>&1)
    hasnt "$out" "can't run under" || return 1
}
check "a run under a sandbox the image can't use warns, and others don't" warns

cd "$root" || exit 1
check "no run labelled or granted the repository" sh -c "! icacls '$root' | grep -q -e 'Mandatory Label' -e 'S-1-15-'"
reg query 'HKCU\Software\QuasidockTest' 2>/dev/null | grep -q . || reg delete 'HKCU\Software\QuasidockTest' /f >/dev/null 2>&1
rm -rf "$work" 2>/dev/null
echo
echo "$failures check(s) failed."
exit $((failures > 0))
