#!/usr/bin/env bash
# zigsaw as a real standard user, which neither a machine's administrator nor
# GitHub's runners are: the first things the README says to do, your real
# folders (safe here, in an account that's only for this), and then the suites
# that don't need an app's image.
#
#   tests/standard-user.sh <account file>
#
# The account file has a user name on its first line and its password on its
# second. The account is a throwaway standard user that exists already: this
# makes none. Its own default store, PATH, Start menu and Documents are used, and
# cleaned up. Whoever runs this needs no rights over the account but the password.
#
# It runs from a copy of what it needs, in a temporary directory (on TMPDIR's
# drive) the account may write, which is deleted afterwards unless KEEP_STAGE is
# set. That account has no Go: the suites' helpers are built here first, and a
# stand-in `go` does nothing when a suite asks for them. SUITES names the
# suites to run ("none" for none). SEED_DOWNLOADS: as the suites take it; only
# files up to 30 MB are copied. Needs Git Bash, Go and network access.
#
# Rows that only say what happened are marked `note`: what Windows lets a
# standard user do decides those, not zigsaw.

set -u
export MSYS_NO_PATHCONV=1
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"

default_suites="store sandbox batch ctrlc network reach install build-base"

if [ "${1:-}" != --inner ]; then
    [ $# -eq 1 ] && [ -f "$1" ] || { echo "usage: tests/standard-user.sh <account file>"; exit 2; }
    account=$(cygpath -aw "$1")
    user=$(head -1 "$1" | tr -d '\r')
    repo=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
    (cd "$repo" && go run ./scripts/build.go all asuser argv-echo hostprobe ctrlc-driver netprobe cred-helper imports acprobe procprobe p256sign) ||
        { echo "building quasidock failed"; exit 1; }
    stage=$(cygpath -wl "$(mktemp -d)")
    icacls "$stage" /grant "$user:(OI)(CI)M" >/dev/null || { echo "$user can't be given $stage"; exit 1; }
    bash "$(cygpath -u "$repo")/scripts/package.sh" "$(cygpath -u "$stage\\package")" >"$stage\\package.log" 2>&1 ||
        { echo "packaging failed: $(tail -1 "$stage\\package.log")"; exit 1; }
    for d in tests recipes scripts go.mod; do cp -r "$(cygpath -u "$repo")/$d" "$(cygpath -u "$stage")/"; done
    mkdir -p "$stage\\out" "$stage\\tmp" "$stage\\bin" "$stage\\seed"
    cp -r "$(cygpath -u "$repo")/out/bin" "$(cygpath -u "$repo")/out/test" "$(cygpath -u "$stage")/out/"
    if [ -n "${SEED_DOWNLOADS:-}" ]; then
        find "$(cygpath -u "$SEED_DOWNLOADS")" -maxdepth 1 -type f -size -30M -exec cp {} "$(cygpath -u "$stage")/seed/" \;
    fi
    # What a suite asks of Go: its helpers, built above.
    printf '#!/bin/sh\nexit 0\n' >"$stage\\bin\\go"
    # A folder that's not the account's: it may write in it, as an ordinary
    # user may in a folder on a drive that lets users modify, and no more.
    mkdir -p "$stage\\not-mine"
    icacls "$stage\\not-mine" /inheritance:r /grant "$user:(OI)(CI)(M)" /grant "$(whoami):(OI)(CI)(F)" >/dev/null

    bash=$(cygpath -w "$(command -v bash)")
    out="$stage\\inner.txt"
    : >"$out"
    "$repo\\out\\test\\quasidock-asuser.exe" "$account" "$out" "\"$bash\" --noprofile --norc \"$stage\\tests\\standard-user.sh\" --inner \"$stage\" \"${SUITES:-$default_suites}\"" &
    runner=$!
    tail -f "$out" &
    tailer=$!
    wait "$runner"
    code=$?
    sleep 1
    kill "$tailer" 2>/dev/null
    # Each suite's rows, of which the above shows only what failed.
    [ -z "${LOG_DIR:-}" ] || cp "$(cygpath -u "$stage")"/*.txt "$(cygpath -u "$LOG_DIR")/" 2>/dev/null
    if [ -z "${KEEP_STAGE:-}" ] && [ "$code" -eq 0 ]; then
        rm -rf "$stage" 2>/dev/null || echo "note: couldn't delete all of $stage"
    else
        echo "kept $stage"
    fi
    exit "$code"
fi

# --- From here on, as the account -------------------------------------------------

export PATH="/usr/bin:/mingw64/bin:$PATH"
stage=$2
suites=$3
own_temp=$TEMP
stage_u=$(cygpath -u "$stage")
export PATH="$stage_u/bin:$PATH"
export TMPDIR="$stage_u/tmp" TMP="$stage\\tmp" TEMP="$stage\\tmp"
export SEED_DOWNLOADS="$stage\\seed"
unset QUASIDOCK_HOME QUASIDOCK_GRANTS_KEY
work="$stage\\tmp\\first"
mkdir -p "$work"
cd "$work" || exit 1

failures=0
check() {
    local label=$1
    shift
    local out
    if out=$("$@" 2>&1); then
        printf 'ok    %s\n' "$label"
    else
        printf 'FAIL  %s\n      %s\n' "$label" "$(grep -v '^\s*$' <<<"$out" | tail -2 | tr '\n' ' ' | cut -c1-200)"
        failures=$((failures + 1))
    fi
}
tell() { printf 'note  %s: %s\n' "$1" "$(tr '\r\n' '  ' <<<"$2" | cut -c1-220)"; }

label_of() { icacls "$1" | grep -o 'Mandatory Label\\[A-Za-z]* Mandatory Level:([A-Z()]*)'; }
low_label() { label_of "$1" | grep -q 'Low Mandatory Level:(OI)(CI)(NW)'; }
no_label() { ! label_of "$1"; }

# The premise: passing means nothing unless this really is a standard user.
is_standard_user() {
    local groups
    groups=$("$SYSTEMROOT\System32\whoami.exe" /groups) || return 1
    ! grep -q 'S-1-5-32-544' <<<"$groups" || { echo "this account is in Administrators"; return 1; }
    grep -q 'Medium Mandatory Level' <<<"$groups" || { echo "this isn't medium integrity: $groups"; return 1; }
    ! net session >/dev/null 2>&1 || { echo "this account may use net session"; return 1; }
}
check "this is $(whoami), a standard user: not in Administrators, at medium integrity" is_standard_user
[ "$failures" -eq 0 ] || { echo "stopping: everything after this would mean nothing."; exit 2; }

docs=$(powershell -NoProfile -Command "[Environment]::GetFolderPath('MyDocuments')" | tr -d '\r')
store="$LOCALAPPDATA\\quasidock"
zigsaw="$store\\bin\\quasidock.exe"
BB=busybox
z() { "$zigsaw" "$@" </dev/null; }
sch="$(cygpath -u "$WINDIR")/System32/schtasks.exe"
task="Zigsaw update check"

# What a run of this that didn't finish left, as the account's deployments are
# read-only.
if [ -e "$store" ]; then
    icacls "$store" /reset /T /C /Q >/dev/null 2>&1
    rm -rf "$store" 2>/dev/null
fi

# What Defender makes of the programs, before anything else: its machine learning
# has flagged builds of zigsaw by their exact bytes (docs/findings.md), and then
# none of them starts, so that every row below would only say the same.
programs_clean() { bash "$stage_u/scripts/defender-scan.sh" "$stage\\package"; }
check "Defender finds nothing in the package's programs" programs_clean
[ "$failures" -eq 0 ] || { echo "stopping: Defender would block what the rest installs."; exit 3; }

# --- The README's first minutes -----------------------------------------------------

installs() {
    powershell -NoProfile -ExecutionPolicy Bypass -File "$stage\\scripts\\install.ps1" -From "$stage\\package" &&
        [ -f "$zigsaw" ] && "$zigsaw" version | grep -q '^quasidock '
}
check "install.ps1 installs it in the default store, with no administrator" installs
on_path() { reg query 'HKCU\Environment' /v Path | grep -qF "$store\\bin"; }
check "  and puts it on this user's PATH" on_path
pulls() { z pull --yes $BB; }
check "pull gets an app from ghcr.io" pulls
for sb in soft low appcontainer; do
    runs_in() { [ "$(qd_run z --sandbox="$1" $BB sh -c 'echo hello; true' | tr -d '\r')" = hello ]; }
    check "it runs under $sb" runs_in "$sb"
done
keeps_data() {
    qd_run z $BB sh -c 'echo kept > "$USERPROFILE/kept.txt"; true' &&
        [ "$(qd_run z $BB sh -c 'cat "$USERPROFILE/kept.txt"; true' | tr -d '\r')" = kept ]
}
check "its data stays between runs" keeps_data

logs_in() {
    # Nothing answers there: the login fails, and Credential Manager has to be
    # reached to save or delete. logout needs nothing but it.
    z logout zigsaw-test.invalid 2>&1 | grep -qv 'error 1312'
}
check "Credential Manager can be reached (a logout)" logs_in

# --- Your folders, for real, as this account's are throwaway ------------------------

unexplained() {
    icacls "$docs" /setintegritylevel '(OI)(CI)L' >/dev/null || return 1
    z grants --take-back "$docs" >/dev/null 2>&1 && no_label "$docs"
}
check "grants --take-back removes a low label" unexplained

# --- A second drive -----------------------------------------------------------------
#
# The stage, on TMPDIR's drive: on C: only if nothing better was given.

own="$stage\\tmp\\my-own"
mkdir -p "$own"
drive_own() {
    qd_run z --filesystem="$own" $BB sh -c "echo x > '$own\\f.txt'; true" && [ -f "$own\\f.txt" ] && no_label "$own"
}
check "a folder it made itself, on ${stage:0:2}, is labelled for a run and taken back" drive_own
not_mine="$stage\\not-mine"
tell "a folder it may modify but doesn't own, on ${stage:0:2}" "$(qd_run z --filesystem="$not_mine" $BB sh -c "echo x > '$not_mine\\f.txt'; echo wrote; true" 2>&1)"

# --- The loopback exemption, which gaps.md said nobody had tried ----------------------

tell "the loopback exemption" "$(qd_run z --verbose  --sandbox=appcontainer --share=network $BB sh -c 'true; true' 2>&1 | grep -iE 'loopback|exempt' | head -3)"
tell "  what grants lists while none runs" "$(z grants $BB)"

# --- A build, for B: under this logon --------------------------------------------------

builds() {
    mkdir -p "$store\\cache\\downloads" && cp "$stage_u"/seed/* "$(cygpath -u "$store")/cache/downloads/" 2>/dev/null
    z build --rebuild "$stage\\recipes\\busybox.json" && ! subst | grep -q '^B:'
}
check "a recipe builds, on its own B:" builds

# --- And away again ---------------------------------------------------------------------

removes() { qd_remove_app z $BB; }
check "rm removes the app" removes
real_clean() {
    no_label "$docs" || { echo "$docs is labelled"; return 1; }
    no_label "$USERPROFILE" || { echo "$USERPROFILE is labelled"; return 1; }
    local out
    out=$(z grants | tr -d '\r')
    [ -z "$out" ] || { echo "grants says: $out"; return 1; }
}
check "nothing it changed is left on your folders" real_clean
uninstalls() {
    powershell -NoProfile -ExecutionPolicy Bypass -File "$stage\\scripts\\install.ps1" -Uninstall &&
        [ ! -e "$zigsaw" ] && ! reg query 'HKCU\Environment' /v Path 2>/dev/null | grep -qF "$store\\bin" &&
        ! reg query 'HKCU\Software\Quasidock\Grants' >/dev/null 2>&1
}
check "the uninstaller removes it, from PATH too, and its record" uninstalls
rm -f "$docs"/su-*.txt
rm -rf "$store"

# --- The suites -----------------------------------------------------------------------

# In the account's own temporary directory, as its programs would be, where it
# may change what it owns as it does in its profile: a second drive that lets
# users only modify keeps icacls from labelling, which rows set up with.
export TMPDIR=$(cygpath -u "$own_temp") TMP="$own_temp" TEMP="$own_temp"
export PACKAGE_DIR="$stage\\package" NEXT_PACKAGE_DIR="$stage\\next-package"
cd "$stage_u" || exit 1
[ "$suites" = none ] && suites=
for s in $suites; do
    echo
    echo "== $s, as $(whoami)"
    start=$SECONDS
    case $s in
    # But for MSVC: Windows' WebCache process, which starts during its build in an
    # account nobody has signed in to, stays and holds files in the build's profile.
    build-base) SECTIONS="runtimes images aliases projects prettier sevenzip" timeout 1200 bash tests/build.sh 2>&1 ;;
    *) timeout 1200 bash "tests/$s.sh" 2>&1 ;;
    esac | tr -d '\r' | tee "$stage\\$s.txt" | grep -E '^FAIL|^      |check\(s\) failed|^[0-9]+ (failed|known)'
    code=${PIPESTATUS[0]}
    echo "$s: exit $code, $(grep -c '^ok ' "$stage\\$s.txt") ok, $(grep -c '^FAIL' "$stage\\$s.txt") failed, $((SECONDS - start)) s"
    # A suite that stopped says why on its last line, which the filter above drops.
    if [ "$code" -ne 0 ]; then
        failures=$((failures + 1))
        tail -2 "$stage\\$s.txt" | cut -c1-200
    fi
done

echo
echo "$failures check(s) failed."
exit $((failures > 0))
