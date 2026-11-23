# What this Windows denies AppContainers, for the suites whose known gaps
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"
# depend on it (tests/matrix.sh, tests/zls.sh). Sourced, not run:
#
#   . "$(dirname "$0")/acprobe.sh"
#   acprobe_run <quasidock.exe> <repository> <work dir>   # sets $probe, or exits
#   want=$(ac_if self-path-dos nul)                     # "ok", or "gap"
#   acprobe_rm <quasidock.exe>
#
# The probe (tests/cmd/acprobe, built with `go run ./scripts/build.go`) is installed
# as an app of its own and run in an AppContainer from a granted directory.
# Its output is a line per check, "<check> ok" or "<check> error <n> (...)".

ACPROBE_ID=test.acprobe

acprobe_run() {
    local quasidock=$1 root=$2 work=$3 out
    (cd "$root" && go run ./scripts/build.go acprobe) || { echo "building the AppContainer probe failed"; exit 1; }
    mkdir -p "$work\\acprobe\\cwd" && cp "$root\\out\\test\\quasidock-acprobe.exe" "$work\\acprobe\\"
    printf '{ "id": "%s", "version": "1", "entrypoint": "quasidock-acprobe.exe", "permissions": { "filesystem": ["cwd"] },\n  "stages": [{ "name": "probe", "sources": [{ "path": "quasidock-acprobe.exe" }] }] }\n' \
        $ACPROBE_ID >"$work\\acprobe\\acprobe.json"
    out=$("$quasidock" build "$work\\acprobe\\acprobe.json" 2>&1) || { echo "$out"; exit 1; }
    probe=$(cd "$work\\acprobe\\cwd" && bash "$QD_IMAGES_HELPER" run "$quasidock" --sandbox=appcontainer $ACPROBE_ID 2>/dev/null | tr -d '\r')
    grep -q '^nul ' <<<"$probe" || { echo "the AppContainer probe didn't run: $probe"; exit 1; }
}

# ac_if <check...>: the intended outcome under AppContainer of a known gap
# whose causes are those probe checks: "ok" where they all passed, else "gap".
ac_if() {
    local check
    for check in "$@"; do
        grep -qx "$check ok" <<<"$probe" || { echo gap; return; }
    done
    echo ok
}

# The probe's report, for the end of a suite.
acprobe_report() {
    echo "In an AppContainer on $(cmd /c ver | tr -d '\r' | grep -v '^$') (tests/cmd/acprobe):"
    grep -v '^#' <<<"$probe" | sed 's/^/  /'
}

acprobe_rm() {
    qd_remove_app "$1" $ACPROBE_ID >/dev/null 2>&1
}
