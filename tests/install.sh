#!/usr/bin/env bash
# End-to-end checks for installing quasidock itself: scripts/package.sh lays
# out a release's files, and scripts/install.ps1 installs them from that
# directory, as it would from a release on GitHub, into a scratch store's bin,
# with a scratch registry key standing in for the user's environment
# variables. It installs over a running app, and removes itself.
#
#   tests/install.sh
#
# Needs Git Bash, Go and Windows PowerShell; downloads nothing. Packages into
# a temporary directory unless PACKAGE_DIR names one that scripts/package.sh
# filled already, as the Release workflow does.

set -u
export MSYS_NO_PATHCONV=1
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"

root=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
work=$(cygpath -wl "$(mktemp -d)")
export QUASIDOCK_HOME="$work\\store"
bin="$QUASIDOCK_HOME\\bin"
# The user's PATH is read and written under this key of HKCU rather than
# Environment.
env_key="Software\\QuasidockTest\\install-$$"
export QUASIDOCK_INSTALL_ENV_KEY="$env_key"
export QUASIDOCK_GRANTS_KEY="Software\\QuasidockTest\\install-$$-grants"
script="$root\\scripts\\install.ps1"
APP=test.install.wait

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

package=$(cygpath -aw "${PACKAGE_DIR:-$work\\package}")
if [ -z "${PACKAGE_DIR:-}" ]; then
    bash "$(cygpath -u "$root")/scripts/package.sh" "$(cygpath -u "$package")" >"$work\\package.log" 2>&1 ||
        { echo "packaging failed: $(tail -1 "$work\\package.log")"; exit 1; }
fi

install() { powershell -NoProfile -ExecutionPolicy Bypass -File "$script" -From "$package" -Dir "$bin" "$@"; }
path_value() { reg query "HKCU\\$env_key" /v Path | tr -d '\r' | grep ' Path '; }
version_go=$(sed -n 's/.*const Version = "\([^"]*\)".*/\1/p' "$root/internal/version/version.go" | head -1)

check "the package has the zip, the installer and their hashes" sh -c \
    "test -f '$package\\quasidock-x86_64-windows.zip' -a -f '$package\\install.ps1' && grep -q 'quasidock-x86_64-windows.zip' '$package\\SHA256SUMS' && grep -q 'install.ps1' '$package\\SHA256SUMS'"
programs_noted() { for f in quasidock.exe quasidock-relay.exe quasidock-host.exe; do grep -q "^| \`$f\` | \`[0-9a-f]\{64\}\` |$" "$package\\notes.md" || return 1; done; }
check "its notes list the programs' hashes" programs_noted

# A PATH as Windows keeps it: referring to another variable.
reg add "HKCU\\$env_key" /v Path /t REG_EXPAND_SZ /d '%USERPROFILE%\tools;C:\other' /f >/dev/null

corrupt="$work\\corrupt"
mkdir -p "$corrupt"
cp "$package\\quasidock-x86_64-windows.zip" "$package\\SHA256SUMS" "$corrupt\\"
printf 'x' >>"$corrupt\\quasidock-x86_64-windows.zip"
refuses_corrupt() {
    local out
    out=$(powershell -NoProfile -ExecutionPolicy Bypass -File "$script" -From "$corrupt" -Dir "$bin" 2>&1) && { echo "it installed"; return 1; }
    tr -d '\r\n' <<<"$out" | grep -q "Nothing was installed" || { echo "$out"; return 1; }
    [ ! -e "$bin\\quasidock.exe" ] && path_value | grep -qF '%USERPROFILE%\tools;C:\other' && ! path_value | grep -qF "$bin"
}
check "a zip that isn't the one SHA256SUMS lists is refused, and nothing changes" refuses_corrupt

installs() {
    local out
    out=$(install 2>&1) || { echo "$out"; return 1; }
    grep -q "^Installed quasidock [0-9][0-9.]* in " <<<"$out" || { echo "$out"; return 1; }
    for f in quasidock.exe quasidock-relay.exe quasidock-host.exe; do [ -f "$bin\\$f" ] || { echo "no $f"; return 1; }; done
    [ "$("$bin\\quasidock.exe" version | tr -d '\r')" = "quasidock $version_go" ]
}
check "it installs quasidock with internal/version/version.go's version" installs
version_info() {
    local version f info
    version=$("$bin\\quasidock.exe" version | tr -d '\r' | cut -d' ' -f2)
    for f in quasidock.exe quasidock-relay.exe quasidock-host.exe; do
        info=$(powershell -NoProfile -Command "\$v = (Get-Item -LiteralPath '$bin\\$f').VersionInfo; \$v.ProductVersion + '|' + \$v.OriginalFilename" | tr -d '\r')
        [ "$info" = "$version|$f" ] || { echo "$f: $info"; return 1; }
        grep -a -q 'level="asInvoker"' "$bin\\$f" || { echo "$f has no manifest"; return 1; }
    done
}
check "each program's version information has quasidock's version, and its manifest runs it as the user" version_info
path_added() {
    path_value | grep -q 'REG_EXPAND_SZ' || { echo "the type changed: $(path_value)"; return 1; }
    path_value | grep -qF "%USERPROFILE%\\tools;C:\\other;$bin" || { path_value; return 1; }
}
check "PATH gains the directory, and stays as it was otherwise: its type, and the variables it names" path_added

"$bin\\quasidock.exe" build "$root\\tests\\install\\app.json" >/dev/null 2>&1 || { echo "building the test app failed"; exit 1; }
bash "$QD_IMAGES_HELPER" run "$bin\\quasidock.exe" $APP 12 &
waiter=$!
sleep 2
upgrades() {
    local out
    out=$(install 2>&1) || { echo "$out"; return 1; }
    ls "$(cygpath -u "$bin")"/quasidock.exe.old-* >/dev/null 2>&1 || { echo "nothing was moved aside: $out"; return 1; }
    "$bin\\quasidock.exe" version >/dev/null
}
check "it installs over a running quasidock, moving the running program aside" upgrades
once() { [ "$(path_value | grep -oF "$bin" | wc -l)" -eq 1 ]; }
check "PATH has the directory once" once
wait $waiter
check "the app that was running ended normally" test $? -eq 0
cleans() { install >/dev/null 2>&1 && ! ls "$(cygpath -u "$bin")"/quasidock.exe.old-* >/dev/null 2>&1; }
check "the next install removes what was moved aside" cleans

# Piped to iex, as the README says, with its settings in the environment.
piped() {
    local out
    out=$(QUASIDOCK_INSTALL_FROM="$package" QUASIDOCK_INSTALL_DIR="$work\\piped" QUASIDOCK_INSTALL_NO_PATH=1 \
        powershell -NoProfile -Command "Get-Content -Raw -LiteralPath '$script' | Invoke-Expression" 2>&1) || { echo "$out"; return 1; }
    [ -f "$work\\piped\\quasidock.exe" ] && ! path_value | grep -qF "$work\\piped"
}
check "piped to Invoke-Expression, it installs where the environment says, and leaves PATH alone when told" piped

refuses_with_apps() {
    local out
    out=$(install -Uninstall 2>&1) && { echo "it uninstalled"; return 1; }
    grep -q "images are still cached:" <<<"$out" && [ -f "$bin\\quasidock.exe" ]
}
check "uninstalling is refused while apps are installed" refuses_with_apps
qd_remove_app "$bin\\quasidock.exe" $APP >/dev/null 2>&1
uninstalls() {
    local out
    out=$(install -Uninstall 2>&1) || { echo "$out"; return 1; }
    for f in quasidock.exe quasidock-relay.exe quasidock-host.exe; do [ ! -e "$bin\\$f" ] || { echo "$f is still there"; return 1; }; done
    path_value | grep -q 'REG_EXPAND_SZ.*%USERPROFILE%\\tools;C:\\other$' || { path_value; return 1; }
}
check "uninstalling removes the programs and the PATH entry, and nothing else of PATH" uninstalls
record_gone() { ! reg query "HKCU\\$QUASIDOCK_GRANTS_KEY" >/dev/null 2>&1; }
check "and the record of what stores changed, which has nothing left" record_gone
reinstall_with_record() {
    local out
    install >/dev/null 2>&1 || return 1
    # Another store, still there, has something recorded.
    reg add "HKCU\\$QUASIDOCK_GRANTS_KEY" /v "$work\\kept|test.other|low $work" /f >/dev/null 2>&1 &&
        mkdir -p "$work\\kept\\grants" && printf 'low %s\n' "$work" >"$work\\kept\\grants\\test.other.txt" || return 1
    out=$(install -Uninstall 2>&1) || { echo "$out"; return 1; }
    grep -q "still has what other stores changed" <<<"$out" && reg query "HKCU\\$QUASIDOCK_GRANTS_KEY" 2>/dev/null | grep -qF 'test.other'
}
check "uninstalling leaves what another store still needs in the record, and says so" reinstall_with_record

reinstall_with_record_subkey() {
    local out
    reg delete "HKCU\\$QUASIDOCK_GRANTS_KEY" /f >/dev/null 2>&1
    install >/dev/null 2>&1 || return 1
    reg add "HKCU\\$QUASIDOCK_GRANTS_KEY\\OtherStore" /v Grant /d kept /f >/dev/null 2>&1 || return 1
    out=$(install -Uninstall 2>&1) || { echo "$out"; return 1; }
    grep -q "still has what other stores changed: OtherStore" <<<"$out" &&
        reg query "HKCU\\$QUASIDOCK_GRANTS_KEY\\OtherStore" /v Grant >/dev/null 2>&1
}
check "uninstalling preserves nonempty record subkeys without prompting" reinstall_with_record_subkey

reg delete "HKCU\\$QUASIDOCK_GRANTS_KEY" /f >/dev/null 2>&1
reg delete "HKCU\\$env_key" /f >/dev/null 2>&1
reg query 'HKCU\Software\QuasidockTest' 2>/dev/null | grep -q . || reg delete 'HKCU\Software\QuasidockTest' /f >/dev/null 2>&1
rm -rf "$work"
echo
echo "$failures check(s) failed."
exit $((failures > 0))
