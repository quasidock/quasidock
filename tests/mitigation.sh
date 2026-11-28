#!/usr/bin/env bash
# Experiment: process mitigation policies on a run's main process. Installs
# tests/cmd/mitprobe as an app and runs it once per mitigation (and once with
# none) under each sandbox, through QUASIDOCK_TEST_MITIGATION and
# QUASIDOCK_TEST_CHILD_POLICY. Prints, per run, the lines that differ from that
# sandbox's run with no mitigation: the policies Windows reports, what the
# probe could still do, and the policies its own child got.
#
#   tests/mitigation.sh [path\to\quasidock.exe] [sandbox...]
#
# Keeps every run's output in $MITIGATION_OUT (default: a temporary dir).
# Always uses a temporary store, and removes it and the probe app afterwards.

set -u
export MSYS_NO_PATHCONV=1
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"

root=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
quasidock=${1:-$root/out/bin/quasidock.exe}
shift $(( $# > 0 ? 1 : 0 ))
sandboxes=("$@")
[ ${#sandboxes[@]} -gt 0 ] || sandboxes=(soft low appcontainer)

QUASIDOCK_HOME=$(cygpath -w "$(mktemp -d)")
export QUASIDOCK_HOME
work=$(cygpath -w "$(mktemp -d)")
out=${MITIGATION_OUT:-$work\\results}
mkdir -p "$out"
suite_key="Software\\QuasidockTest\\mitigation-$$-grants"
export QUASIDOCK_GRANTS_KEY="$suite_key"
ID=test.mitprobe
cleanup() {
    qd_remove_app "$quasidock" $ID >/dev/null 2>&1
    reg delete "HKCU\\$suite_key" /f >/dev/null 2>&1
    rm -rf "$QUASIDOCK_HOME" "$work\\app" "$work\\cwd"
}
trap cleanup EXIT

(cd "$root" && go run ./scripts/build.go mitprobe) || { echo "building the probe failed"; exit 1; }

sys32='C:\Windows\System32'
mkdir -p "$work\\app" "$work\\cwd"
cp "$root\\out\\test\\quasidock-mitprobe.exe" "$work\\app\\"
cp "$sys32\\version.dll" "$work\\app\\version.dll"
printf '{ "id": "%s", "version": "1", "entrypoint": "quasidock-mitprobe.exe", "permissions": { "filesystem": ["cwd"] },\n  "stages": [{ "name": "probe", "sources": [{ "path": "quasidock-mitprobe.exe" }, { "path": "version.dll" }] }] }\n' \
    $ID >"$work\\app\\mitprobe.json"
built=$("$quasidock" build "$work\\app\\mitprobe.json" 2>&1) || { echo "$built"; exit 1; }

# The image checks' files: a Microsoft-signed copy, the same with its PE
# TimeDateStamp changed (so its hash matches no catalog), and a Low-labeled copy.
cp "$sys32\\version.dll" "$work\\cwd\\mitprobe-ms.dll"
cp "$sys32\\version.dll" "$work\\cwd\\mitprobe-unsigned.dll"
pe=$(od -An -tu4 -j60 -N4 "$work\\cwd\\mitprobe-unsigned.dll" | tr -d ' ')
printf '\x51\x44\x51\x44' | dd of="$work\\cwd\\mitprobe-unsigned.dll" bs=1 seek=$((pe + 8)) conv=notrunc 2>/dev/null
cp "$sys32\\version.dll" "$work\\cwd\\mitprobe-low.dll"
icacls "$work\\cwd\\mitprobe-low.dll" /setintegritylevel L >/dev/null
cp 'C:\Windows\Fonts\arial.ttf' "$work\\cwd\\mitprobe-font.ttf"
powershell -NoProfile -Command "Get-ChildItem '$work\\cwd\\*.dll' | ForEach-Object { '# ' + \$_.Name + ' ' + (Get-AuthenticodeSignature \$_.FullName).Status }" | tr -d '\r'
export MITPROBE_REMOTE='\\localhost\C$\Windows\System32\version.dll'

# name policy policy2 childpolicy
mitigations=(
    "none 0 0 0"
    "force-relocate $((1 << 8)) 0 0"
    "heap-terminate $((1 << 12)) 0 0"
    "bottom-up-aslr $((1 << 16)) 0 0"
    "high-entropy-aslr $((1 << 20)) 0 0"
    "strict-handles $((1 << 24)) 0 0"
    "no-win32k $((1 << 28)) 0 0"
    "no-extension-points $((1 << 32)) 0 0"
    "no-dynamic-code $((1 << 36)) 0 0"
    "cfg $((1 << 40)) 0 0"
    "cfg+strict $((1 << 40)) $((1 << 8)) 0"
    "microsoft-signed $((1 << 44)) 0 0"
    "no-nonsystem-fonts $((1 << 48)) 0 0"
    "no-remote-images $((1 << 52)) 0 0"
    "no-low-label-images $((1 << 56)) 0 0"
    "prefer-system32 $((1 << 60)) 0 0"
    "loader-integrity 0 $((1 << 4)) 0"
    "strict-cfg 0 $((1 << 8)) 0"
    "module-tampering 0 $((1 << 12)) 0"
    "restrict-ibp 0 $((1 << 16)) 0"
    "ssb-disable 0 $((1 << 24)) 0"
    "cet-shadow-stacks 0 $((1 << 28)) 0"
    "cet-strict 0 $((3 << 28)) 0"
    "cet-setcontext-ip 0 $((1 << 32)) 0"
    "block-non-cet 0 $((1 << 36)) 0"
    "xfg 0 $((1 << 40)) 0"
    "cet-dynamic-apis-oop 0 $((1 << 48)) 0"
    "restrict-core-sharing 0 $((1 << 52)) 0"
    "no-fsctl 0 $((1 << 56)) 0"
    "no-child-processes 0 0 1"
)

for sandbox in "${sandboxes[@]}"; do
    echo "== $sandbox"
    for entry in "${mitigations[@]}"; do
        read -r name p1 p2 child <<<"$entry"
        file="$out\\$sandbox-$name.txt"
        result=$(cd "$work\\cwd" && QUASIDOCK_TEST_MITIGATION="$p1,$p2" QUASIDOCK_TEST_CHILD_POLICY="$child" \
            "$quasidock" run --rm --sandbox="$sandbox" -e MITPROBE_REMOTE $ID 2>&1 </dev/null)
        code=$?
        # A Go crash keeps only its exception line, not every goroutine's stack.
        result=$(tr -d '\r' <<<"$result" | grep -v '^note: ' | sed '/^Exception 0x/q')
        printf '%s\n# exit %d\n' "$result" "$code" >"$file"
        if [ "$name" = none ]; then
            echo "  none: exit $code$(grep -q '^# done' "$file" || echo ', INCOMPLETE')"
            continue
        fi
        changed=$(diff <(grep -v '^# exit' "$out\\$sandbox-none.txt") <(grep -v '^# exit' "$file") | grep '^[<>]')
        # A run that never started: what it said, not the whole baseline.
        grep -q '^policy ' "$file" || changed=$(grep '^>' <<<"$changed")
        changed=$(sed 's/^/      /' <<<"$changed")
        echo "  $name: exit $code$(grep -q '^# done' "$file" || echo ', INCOMPLETE')"
        [ -z "$changed" ] || printf '%s\n' "$changed"
    done
done
echo "outputs: $out"
