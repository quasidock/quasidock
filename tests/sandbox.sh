#!/usr/bin/env bash
# End-to-end checks for what the enforcing sandboxes change on the host, how
# long it lasts, and what `zigsaw rm` undoes: low integrity labels on the
# paths a low run (the default) may write, access granted to an app's
# AppContainer, and what each lets a run write; which sandbox a run gets
# (the image's, the overrides', the command line's). What a run's command
# line asks for lasts until the app's last run ends; what its permissions and
# saved overrides give stays (src/grants.zig). Busybox apps; tests/matrix.sh
# runs real tools.
#
#   tests/sandbox.sh [path\to\quasidock.exe]
#
# Needs Git Bash and network access (for the busybox download, unless
# SEED_DOWNLOADS has it). Always uses a temporary store (on TMPDIR's drive),
# and removes it, and the AppContainer profiles it made, afterwards.

set -u
export MSYS_NO_PATHCONV=1
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"

root=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
zigsaw=${1:-$root/out/bin/quasidock.exe}
QUASIDOCK_HOME=$(cygpath -w "$(mktemp -d)")
export QUASIDOCK_HOME
work=$(cygpath -w "$(mktemp -d)")
# The record of what stores changed (src/ledger.zig): one of this run's own.
suite_key="Software\\QuasidockTest\\sandbox-$$-grants"
export QUASIDOCK_GRANTS_KEY="$suite_key"
trap 'reg delete "HKCU\\$suite_key" /f >/dev/null 2>&1' EXIT
# SEED_DOWNLOADS: another store's cache\downloads, whose files are linked, or
# copied, into this one's, so as not to download them again.
if [ -n "${SEED_DOWNLOADS:-}" ]; then
    mkdir -p "$QUASIDOCK_HOME\\cache\\downloads"
    cp -l "$(cygpath -u "$SEED_DOWNLOADS")"/* "$(cygpath -u "$QUASIDOCK_HOME")/cache/downloads/" 2>/dev/null ||
        cp -n "$(cygpath -u "$SEED_DOWNLOADS")"/* "$(cygpath -u "$QUASIDOCK_HOME")/cache/downloads/"
fi

BB=busybox
OTHER=test.sandbox.other

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
fails() { ! "$@"; }

z() { "$zigsaw" "$@" </dev/null; }
# writes <app> <sandbox> <file> [run options...]: whether a run can create the file.
writes() {
    local app=$1 sb=$2 file=$3
    shift 3
    qd_run z --sandbox="$sb" "$@" "$app" sh -c "echo x > '$file'"
}
# What icacls says about a path's integrity label, and its ACEs for
# AppContainer packages (S-1-15-2-) and capabilities (S-1-15-3-).
label_of() { icacls "$1" | grep -o 'Mandatory Label\\[A-Za-z]* Mandatory Level:([A-Z()]*)'; }
low_label() { label_of "$1" | grep -q 'Low Mandatory Level:(OI)(CI)(NW)'; }
no_label() { ! label_of "$1"; }
has_ace() { icacls "$1" | grep -q "$2"; }
granted() { grep -qxF "$2" "$QUASIDOCK_HOME\\grants\\$1.txt" 2>/dev/null; }
# icacls_in <app> <sandbox> <path> [run options...]: icacls's view of the
# path from inside a run of the app, while what the run was granted is there.
icacls_in() {
    local app=$1 sb=$2 p=$3
    shift 3
    qd_run z --sandbox="$sb" "$@" --entrypoint=icacls "$app" "$p"
}
# hold <dir> <app> [run options...]: starts a run of the app that goes on,
# writing to <dir>, until `release`; returns once it's going. Its PID is in
# $held.
hold() {
    local dir=$1 app=$2
    shift 2
    rm -f "$dir\\started" "$dir\\stop"
    bash "$QD_IMAGES_HELPER" run "$zigsaw" "$@" "$app" sh -c "touch '$dir\\started'; while [ ! -f '$dir\\stop' ]; do sleep 0.2; done" </dev/null >/dev/null 2>&1 &
    held=$!
    held_dir=$dir
    local i
    for i in $(seq 100); do [ -f "$dir\\started" ] && return 0; sleep 0.1; done
    return 1
}
release() {
    touch "$held_dir\\stop"
    wait $held
}

(cd "$root" && go run ./scripts/build.go hostprobe) || { echo "building the host probe failed"; exit 1; }
hostprobe="$root\\out\\test\\quasidock-hostprobe.exe"
"$zigsaw" build "$root/recipes/busybox.json" >/dev/null 2>&1 || { echo "building busybox failed"; exit 1; }
sed "s/\"busybox\"/\"$OTHER\"/" "$root/recipes/busybox.json" >"$work\\other.json"
"$zigsaw" build "$work\\other.json" >/dev/null 2>&1 || { echo "building the second app failed"; exit 1; }

mkdir -p "$work\\rw" "$work\\ro" "$work\\outside" "$work\\cwd" "$work\\ac" "$work\\mine" "$work\\own"
echo secret >"$work\\outside\\secret.txt"

# --- What a low run can write ------------------------------------------------
#
# Grants from a run's command line last for the run: what it has meanwhile
# shows from inside it.

labelled_in_run() { icacls_in $BB low "$1" --filesystem="$2" | grep -qF "$3"; }
label_gone() { no_label "$1" && fails granted $BB "low $1"; }
check "low: writes its data directory" qd_run z --sandbox=low $BB sh -c 'echo x > "$APPDATA/f"'
check "low: writes a path it may write" writes $BB low "$work\\rw\\f" --filesystem="$work\\rw"
check "  which is labelled low while it runs, for everything in it" labelled_in_run "$work\\rw" "$work\\rw" 'Low Mandatory Level:(OI)(CI)(NW)'
check "  what it wrote inherits the label" labelled_in_run "$work\\rw\\f" "$work\\rw" 'Low Mandatory Level:(I)(NW)'
check "  and the label, and its record, go when the run ends" sh -c "$(declare -f label_of no_label fails granted label_gone); BB=$BB; QUASIDOCK_HOME='$QUASIDOCK_HOME'; label_gone '$work\\rw' && no_label '$work\\rw\\f'"
check "low: can't write a read-only grant" fails writes $BB low "$work\\ro\\f" --filesystem="$work\\ro:ro"
check "  which isn't labelled" no_label "$work\\ro"
check "low: can't write elsewhere" fails writes $BB low "$work\\outside\\f"
check "low: reads elsewhere (not enforced)" qd_run z --sandbox=low $BB cat "$work\\outside\\secret.txt"
check "low: writes cwd with the cwd permission" sh -c "cd '$work\\cwd' && bash '$QD_IMAGES_HELPER' run \"\$0\" --name low-cwd --sandbox=low --filesystem=cwd $BB sh -c 'echo x > \"\$1\"' sh '$work\\cwd\\f' </dev/null" "$zigsaw"
check "  which, given on the command line, isn't labelled after it" label_gone "$work\\cwd"
check "  and the run's data directory is labelled" low_label "$(qd_container_data "$zigsaw" low-cwd)"
refuses_profile() {
    local out
    out=$(writes $BB low "$USERPROFILE\\quasidock-sandbox-test" --filesystem="$USERPROFILE" 2>&1) && return 1
    grep -q "it's your user profile" <<<"$out" || { echo "$out"; return 1; }
    [ ! -e "$USERPROFILE\\quasidock-sandbox-test" ] && no_label "$USERPROFILE"
}
check "low: won't label the user profile" refuses_profile
check "low: --rm writes its own data directory" qd_run z --sandbox=low --rm $BB sh -c 'echo x > "$APPDATA/f"'
# BusyBox's sh waits for the last command of sh -c only if it can see its
# parent, zigsaw, which a low-integrity process could not read.
check "low: sh sees zigsaw as its parent" sh -c '[ "$(bash "$QD_IMAGES_HELPER" run "$0" --sandbox=low '$BB' sh -c "echo \$PPID" </dev/null | tr -d "\r")" != 1 ]' "$zigsaw"
check "low: sh -c's last command runs to the end, and its exit code comes through" sh -c '
    out=$(bash "$QD_IMAGES_HELPER" run "$0" --sandbox=low '$BB' sh -c "busybox.exe sh -c \"sleep 1; echo late; exit 7\"" </dev/null | tr -d "\r"; echo "code ${PIPESTATUS[0]}")
    [ "$out" = "late
code 7" ]' "$zigsaw"

# A directory that gives its owner Modify, not WRITE_OWNER, which labelling
# needs, as a fresh drive's root does (D:\ on Windows 11). zigsaw grants it
# for the change, and puts the ACL back.
mkdir -p "$work\\modify\\rw"
icacls "$work\\modify" /inheritance:r /grant:r "$USERNAME:(OI)(CI)M" >/dev/null
acl_before=$(icacls "$work\\modify\\rw" | grep -v '^Successfully')
if icacls "$work\\modify\\rw" /setintegritylevel '(OI)(CI)low' >/dev/null 2>&1; then
    # An administrator's token can take ownership; CI runs as one.
    echo "skip  low: a path that withholds WRITE_OWNER (this user can label it anyway)"
else
    check "low: writes a path that withholds WRITE_OWNER" writes $BB low "$work\\modify\\rw\\f" --filesystem="$work\\modify\\rw"
    check "  which is labelled low while it runs" labelled_in_run "$work\\modify\\rw" "$work\\modify\\rw" 'Low Mandatory Level:(OI)(CI)(NW)'
    check "  and unlabelled after it, with its ACL as it was" sh -c "[ \"\$(icacls '$work\\modify\\rw' | grep -v '^Successfully')\" = \"\$0\" ]" "$acl_before"
fi

# --- The default sandbox ----------------------------------------------------------
#
# Low, unless the app's image asks for another, or an override or the command
# line chooses one: the app's overrides over the store-wide ones (--global).

SOFT=test.sandbox.soft
sed -e "s/\"busybox\"/\"$SOFT\"/" \
    -e 's/"entrypoint": "busybox.exe",/&\n  "permissions": { "sandbox": "soft" },/' \
    "$root/recipes/busybox.json" >"$work\\soft.json"
soft_built=$("$zigsaw" build "$work\\soft.json" 2>&1 </dev/null) || { echo "building the soft app failed: $soft_built"; exit 1; }
mkdir -p "$work\\global"
# from <dir> <run options and app...>: a run that may write its working
# directory tries to, from <dir>; prints what zigsaw said.
from() {
    local d=$1
    shift
    (cd "$d" && bash "$QD_IMAGES_HELPER" run "$zigsaw" --filesystem=cwd "$@" sh -c 'ls >/dev/null && echo x > quasidock-sandbox-test' </dev/null 2>&1)
}
# read_only_cwd <dir>: a low run from there goes on, with its working
# directory read-only, and says so; nothing is written or labelled.
read_only_cwd() {
    local out
    out=$(from "$1" $BB) && return 1
    grep -q "is read-only for this run, as $2: run the app from a folder inside it" <<<"$out" || { echo "$out"; return 1; }
    [ ! -e "$1\\quasidock-sandbox-test" ] && fails low_label "$1" && fails granted $BB "low $1"
}
drive=${work:0:3}
home_run_quiet() {
    qd_run z --name home-ref $BB true >/dev/null || return 1
    local home out
    home="$(qd_container_data "$zigsaw" home-ref)\\home"
    out=$(from "$home" $BB) || { echo "$out"; return 1; }
    ! grep -q labelled <<<"$out" && fails granted $BB "low $home" && [ ! -e "$QUASIDOCK_HOME\\grants\\$BB.cwd" ]
}
check "a run without --sandbox is low: it writes its data directory" qd_run z $BB sh -c 'echo x > "$APPDATA/default"'
check "  and can't write elsewhere" fails qd_run z $BB sh -c "echo x > '$work\\outside\\default'"
check "low: from your user profile, the app runs with its working directory read-only" read_only_cwd "$USERPROFILE" "it's your user profile"
check "low: from a drive's root, too" read_only_cwd "$drive" "it's the root of a drive"
check "low: from the app's own home, nothing is labelled or recorded" home_run_quiet
check "an image that asks for soft says so when installed" grep -q 'permits  whatever you can write (it asks for the soft sandbox)' <<<"$soft_built"
check "  and its runs write elsewhere" qd_run z $SOFT sh -c "echo x > '$work\\outside\\soft1'"
check "  and the command line (--sandbox=low) over the image's" fails writes $SOFT low "$work\\outside\\soft6"
qd_remove_app "$zigsaw" $SOFT >/dev/null 2>&1 </dev/null

# --- AppContainer grants, and a low run after one ------------------------------

capability_in_run() {
    local out
    out=$(icacls_in $BB appcontainer "$1" --filesystem="$1") || { echo "$out"; return 1; }
    grep -q 'S-1-15-3-1024-' <<<"$out" && ! grep -q 'S-1-15-2-' <<<"$out"
}
ace_gone() { fails has_ace "$1" 'S-1-15-' && fails granted $BB "$1"; }
check "appcontainer: writes a granted path" writes $BB appcontainer "$work\\ac\\f" --filesystem="$work\\ac"
check "  granted to the app's capability while it runs, not its package SID" capability_in_run "$work\\ac"
check "  and revoked when the run ends" ace_gone "$work\\ac"
check "low after appcontainer: runs, and reads what that run wrote" qd_run z --sandbox=low --filesystem="$work\\ac" $BB cat "$work\\ac\\f"
check "appcontainer: can't write elsewhere" fails writes $BB appcontainer "$work\\outside\\f"

# --- While another run of the app goes on --------------------------------------
#
# The last run of an app to end takes back what its runs' command lines
# asked for: a second run that hands a file to the first, as Text Editor's
# does, leaves the first able to open it.

killed_leaves() {
    hold "$work\\rw" $BB --sandbox=low --filesystem="$work\\rw" || return 1
    kill -9 $held
    touch "$work\\rw\\stop"
    wait $held 2>/dev/null
    low_label "$work\\rw"
}
check "a killed run leaves what it was granted" killed_leaves
check "  which the app's next run takes back when it ends" sh -c "bash '$QD_IMAGES_HELPER' run \"\$0\" $BB true </dev/null && $(declare -f label_of no_label fails granted label_gone); BB=$BB; QUASIDOCK_HOME='$QUASIDOCK_HOME'; label_gone '$work\\rw'" "$zigsaw"

# --- What stands --------------------------------------------------------------
#
# A third app, with overrides saved: what a run with nothing on its command
# line would be granted stays, until the overrides no longer give it.

mkdir -p "$work\\c1"
in_dir() {
    local d=$1
    shift
    (cd "$d" && "$zigsaw" "$@" </dev/null)
}
cmdline_cwd() {
    in_dir "$work\\c1" run --entrypoint=busybox.exe --sandbox=low --filesystem=cwd $BB sh -c 'echo y > f' &&
        no_label "$work\\c1" && [ ! -e "$QUASIDOCK_HOME\\grants\\$BB.cwd" ]
}
check "cwd: one a command-line --filesystem=cwd grants goes after its run" cmdline_cwd

# --- The user's folders (--filesystem=documents) -------------------------------
#
# Fake folders: QUASIDOCK_USER_FOLDERS has zigsaw take <dir>\Documents and so on
# for the user's. A fourth app declares documents, and a permission of a later
# zigsaw's.

FOLD=test.sandbox.folders
sed -e "s/\"busybox\"/\"$FOLD\"/" \
    -e 's/"entrypoint": "busybox.exe",/&\n  "permissions": { "filesystem": ["cwd", "xdg-later"] },/' \
    "$root/recipes/busybox.json" >"$work\\folders.json"
"$zigsaw" build "$work\\folders.json" >/dev/null 2>&1 || { echo "building the folders app failed"; exit 1; }
warns_unknown() { bash "$QD_IMAGES_HELPER" run "$zigsaw" $FOLD true 2>&1 </dev/null | grep -q 'xdg-later.*doesn.t know'; }
check "  a permission this quasidock doesn't know is left out, with a warning" warns_unknown
qd_remove_app "$zigsaw" $FOLD >/dev/null 2>&1 </dev/null

# --- Registry keys an app keeps (its config's "registry") --------------------------
#
# A fifth app keeps a test key, and a sixth the same one. Their runs set and
# read values with System32's reg.exe (--entrypoint=reg).

REG=test.sandbox.registry
REG2=test.sandbox.registry2
key="Software\\QuasidockTest\\sandbox-$$"
record="HKCU\\Software\\quasidock\\keys\\Software/QuasidockTest/sandbox-$$"
for app in $REG $REG2; do
    sed -e "s/\"busybox\"/\"$app\"/" \
        -e "s/\"entrypoint\": \"busybox.exe\",/&\n  \"registry\": [\"Software\\\\\\\\QuasidockTest\\\\\\\\sandbox-$$\"],/" \
        "$root/recipes/busybox.json" >"$work\\$app.json"
    "$zigsaw" build "$work\\$app.json" >/dev/null 2>&1 || { echo "building $app failed"; exit 1; }
done
# sets <sandbox> <name> [run options...]: a run sets a value in the key.
sets() {
    local sb=$1 name=$2
    shift 2
    qd_run z --sandbox="$sb" "$@" --entrypoint=reg $REG add "HKCU\\$key" /v "$name" /d "$sb" /f
}
# has <sandbox> <name> [run options...]: a run finds the value.
has() {
    local sb=$1 name=$2
    shift 2
    qd_run z --sandbox="$sb" "$@" --entrypoint=reg $REG query "HKCU\\$key" /v "$name"
}
not_in_hkcu() { ! reg query "HKCU\\$key" >/dev/null 2>&1 && ! reg query "$record" >/dev/null 2>&1; }
in_order() {
    local sb
    for sb in soft low appcontainer; do
        qd_run z --sandbox="$sb" --name "reg-$sb" --entrypoint=cmd $REG /c "reg add HKCU\\$key /v $sb /d $sb /f && reg query HKCU\\$key /v $sb" || { echo "$sb registry write/query failed"; return 1; }
    done
}
rm_leaves_nothing() {
    ! z container inspect reg-rm >/dev/null 2>&1 &&
        qd_run z --sandbox=soft --rm --name reg-rm --entrypoint=cmd $REG /c "reg add HKCU\\$key /v e /d soft /f >NUL && reg query HKCU\\$key /v e >NUL" &&
        ! z container inspect reg-rm >/dev/null 2>&1 && not_in_hkcu
}
moved_aside() {
    reg add "HKCU\\$key" /v native /d 1 /f >/dev/null || return 1
    fails has soft native && sets soft a || { reg delete "HKCU\\$key" /f >/dev/null 2>&1; return 1; }
    reg query "HKCU\\$key" /v native >/dev/null && ! reg query "HKCU\\$key" /v a >/dev/null 2>&1
    local status=$?
    reg delete "HKCU\\$key" /f >/dev/null 2>&1
    return $status
}
# Runs `sleep` in the app in the background, and waits until its run has put
# the key in HKCU; its PID in $runner.
start_sleeping() {
    bash "$QD_IMAGES_HELPER" run "$zigsaw" "$1" sleep 60 </dev/null >/dev/null 2>&1 &
    runner=$!
    local i
    for i in $(seq 50); do reg query "$record" >/dev/null 2>&1 && return 0; sleep 0.2; done
    return 1
}
killed_run() {
    start_sleeping $REG || return 1
    # Set as the app would, while its run goes on.
    reg add "HKCU\\$key" /v killed /d 1 /f >/dev/null
    kill -9 $runner
    wait $runner 2>/dev/null
    reg query "$record" >/dev/null 2>&1 || { echo "the record went with the run"; return 1; }
    sets soft settled && not_in_hkcu
}
other_app_waits() {
    start_sleeping $REG || return 1
    local out
    out=$(qd_run z --entrypoint=reg $REG2 query "HKCU\\$key" 2>&1)
    local status=$?
    kill -9 $runner
    wait $runner 2>/dev/null
    [ $status -ne 0 ] && grep -q "which a run of $REG is using" <<<"$out" || { echo "$out"; return 1; }
    # Its next run settles what the killed one left.
    qd_run z $REG2 true >/dev/null 2>&1 && not_in_hkcu
}
rm_settles() {
    start_sleeping $REG || return 1
    kill -9 $runner
    wait $runner 2>/dev/null
    qd_remove_app "$zigsaw" $REG >/dev/null 2>&1 </dev/null && not_in_hkcu && ! z container inspect reg-soft >/dev/null 2>&1
}

check "registry: a container reads what it sets, in each sandbox" in_order
check "  in a hive in the container's data" test -f "$(qd_container_data "$zigsaw" reg-soft)\\registry\\user.dat"
check "  and HKCU has neither the key nor zigsaw's record of it between runs" not_in_hkcu
check "--rm: runs with its own keys and leaves nothing behind" rm_leaves_nothing
check "a key of the same name that was there is moved aside for the run, and back after" moved_aside
check "a run that didn't end leaves the key in HKCU, and the next one settles it" killed_run
check "another app keeping the same key can't run while that app's run uses it" other_app_waits
check "rm settles what a run left, and leaves nothing in HKCU" rm_settles
qd_remove_app "$zigsaw" $REG2 >/dev/null 2>&1 </dev/null
reg delete "HKCU\\$key" /f >/dev/null 2>&1

# A seventh app keeps a key two levels below one that's there and empty, as
# Java apps keep theirs below an empty HKCU\Software\JavaSoft\Prefs: taking
# it back deletes the key its run made above it, not the one that was there.
REG3=test.sandbox.registry3
deep="Software\\QuasidockTest\\deep-$$"
sed -e "s/\"busybox\"/\"$REG3\"/" \
    -e "s/\"entrypoint\": \"busybox.exe\",/&\n  \"registry\": [\"Software\\\\\\\\QuasidockTest\\\\\\\\deep-$$\\\\\\\\made\\\\\\\\settings\"],/" \
    "$root/recipes/busybox.json" >"$work\\$REG3.json"
"$zigsaw" build "$work\\$REG3.json" >/dev/null 2>&1 || { echo "building $REG3 failed"; exit 1; }
only_what_it_made() {
    # Without values, as Java leaves Prefs: reg.exe would give it an empty
    # default value.
    powershell -NoProfile -Command "New-Item -Path 'HKCU:\\$deep' -Force | Out-Null" || return 1
    qd_run z --name deep-reg --entrypoint=reg $REG3 add "HKCU\\$deep\\made\\settings" /v x /d 1 /f || return 1
    reg query "HKCU\\$deep" >/dev/null 2>&1 || { echo "the key that was there went"; return 1; }
    ! reg query "HKCU\\$deep\\made" >/dev/null 2>&1 || { echo "the key its run made stayed"; return 1; }
    test -f "$(qd_container_data "$zigsaw" deep-reg)\\registry\\user.dat"
}
check "taking a key back deletes the keys above it its run made, not an empty one that was there" only_what_it_made
qd_remove_app "$zigsaw" $REG3 >/dev/null 2>&1 </dev/null
reg delete "HKCU\\$deep" /f >/dev/null 2>&1


# --- Labels two apps share, labels zigsaw didn't make, and rm ---------------------
#
# Both apps' overrides choose low and name the same path, so its label
# stands for both.

icacls "$work\\mine" /setintegritylevel '(OI)(CI)low' >/dev/null
check "a path labelled low by hand is written" writes $BB low "$work\\mine\\f" --filesystem="$work\\mine"
check "  but not recorded" fails granted $BB "low $work\\mine"
check "  and keeps its label after the run" low_label "$work\\mine"
qd_remove_app "$zigsaw" $BB >/dev/null 2>&1 </dev/null
check "rm leaves a label it didn't make" low_label "$work\\mine"
qd_remove_app "$zigsaw" $OTHER >/dev/null 2>&1 </dev/null

# --- Programs a run writes, and zigsaw's own folders ---------------------------
#
# Windows starts a program whose file is labelled low at low integrity, and
# what a run writes comes out labelled low: zigsaw takes the label off the
# programs once the app's runs end (src/written.zig). tests/cmd/hostprobe is
# the program: it prints its integrity level.

PROG=test.sandbox.programs
sed "s/\"busybox\"/\"$PROG\"/" "$root/recipes/busybox.json" >"$work\\programs.json"
"$zigsaw" build "$work\\programs.json" >/dev/null 2>&1 || { echo "building the fourth app failed"; exit 1; }
mkdir -p "$work\\prog" "$work\\progac" "$work\\lowexe"
cp "$hostprobe" "$work\\prog\\src.bin" && cp "$hostprobe" "$work\\progac\\src.bin"

# starts_at <level> <program>: its own integrity, and its label's.
starts_at() {
    local out
    out=$("$2" 2>&1 | tr -d '\r' | grep '^# integrity')
    [ "$out" = "# integrity $1" ] || { echo "$2: $out"; return 1; }
}
medium_program() { starts_at medium "$1" && icacls "$1" | grep -q 'Medium Mandatory Level:(NW)'; }
recorded_program() { grep -qxF "$2" "$QUASIDOCK_HOME\\grants\\$1.programs"; }
writes_program() {
    local app=$1 to=$2
    shift 2
    qd_run z "$@" $app sh -c "cp '$(dirname "$to")\\src.bin' '$to'" >/dev/null 2>&1
}
rewrites_in_place() {
    qd_run z --filesystem="$work\\prog" $PROG sh -c "cat '$work\\prog\\src.bin' > '$work\\prog\\p.exe'" && medium_program "$work\\prog\\p.exe"
}
held_stays_low() {
    hold "$work\\prog" $PROG --filesystem="$work\\prog" || return 1
    writes_program $PROG "$work\\prog\\h.exe" --filesystem="$work\\prog"
    if ! starts_at low "$work\\prog\\h.exe"; then
        release
        return 1
    fi
    release
    medium_program "$work\\prog\\h.exe"
}
rm_forgets() {
    qd_remove_app "$zigsaw" $PROG >/dev/null 2>&1 </dev/null || return 1
    [ ! -e "$QUASIDOCK_HOME\\grants\\$PROG.programs" ] && medium_program "$work\\prog\\p.exe"
}
store_parent=$(dirname "$QUASIDOCK_HOME")
refuses_store() {
    local out
    out=$(writes $PROG "$1" "$store_parent\\quasidock-sandbox-test" --filesystem="$store_parent" 2>&1) && return 1
    grep -q "quasidock's store is in it" <<<"$out" || { echo "$out"; return 1; }
    [ ! -e "$store_parent\\quasidock-sandbox-test" ] && no_label "$store_parent" && fails has_ace "$store_parent" 'S-1-15-3-'
}
in_store_read_only() {
    local d="$QUASIDOCK_HOME\\cache" out
    out=$(from "$d" $PROG) && return 1
    grep -q "is read-only for this run, as it's in quasidock's store: run the app from a folder elsewhere" <<<"$out" || { echo "$out"; return 1; }
    [ ! -e "$d\\quasidock-sandbox-test" ] && no_label "$d"
}
warns_low_program() {
    icacls "$work\\lowexe" /setintegritylevel '(OI)(CI)L' >/dev/null && cp "$hostprobe" "$work\\lowexe\\probe.exe" || return 1
    local out
    out=$(qd_run z --sandbox=soft --entrypoint="$work\\lowexe\\probe.exe" $PROG 2>&1)
    grep -q 'is labelled low integrity, so Windows starts it, and this run, at low integrity' <<<"$out" || { echo "$out"; return 1; }
}
low_writes() { writes_program $PROG "$work\\prog\\p.exe" --filesystem="$work\\prog" && medium_program "$work\\prog\\p.exe"; }
soft_writes() {
    writes_program $PROG "$work\\prog\\s.exe" --sandbox=soft || return
    local expected out
    expected=$("$hostprobe" | tr -d '\r' | grep '^# integrity')
    out=$("$work\\prog\\s.exe" | tr -d '\r' | grep '^# integrity')
    [ "$out" = "$expected" ] || { echo "got $out, want $expected"; return 1; }
}
ac_writes() { writes_program $PROG "$work\\progac\\a.exe" --sandbox=appcontainer --filesystem="$work\\progac" && medium_program "$work\\progac\\a.exe"; }
check "low: a program a run writes where it may write starts at medium integrity after" low_writes
check "  recorded" recorded_program $PROG "$work\\prog\\p.exe"
check "  and the app's next low run rewrites it in place, and it's medium again" rewrites_in_place
check "soft: a program written in a folder starts at the caller's normal integrity" soft_writes
check "appcontainer: a program written in a granted path starts at medium too" ac_writes
check "while another run of the app goes on, a program stays low until that one ends" held_stays_low
check "low: from inside the store, the app runs with its working directory read-only" in_store_read_only
check "low: won't label a folder that holds the store" refuses_store low
check "appcontainer: won't let the app write a folder that holds the store" refuses_store appcontainer
check "soft: a run of a program labelled low says it starts at low integrity" warns_low_program

# Each container owns its data. A low container labels only its own tree;
# a concurrent soft container gets a separate tree and its normal integrity.
data_low_written() {
    local data_low
    qd_run z --name data-low $PROG sh -c 'cp "$1" "$HOME/../d.exe"' sh "$work\\prog\\src.bin" >/dev/null 2>&1 || return 1
    data_low=$(qd_container_data "$zigsaw" data-low) || return
    low_label "$data_low" && starts_at low "$data_low\\d.exe"
}
data_soft_written() {
    local out data_low data_soft
    out=$(qd_run z --name data-soft --sandbox=soft $PROG sh -c 'cp "$1" "$HOME/../s.exe" && "$HOME/../s.exe"' sh "$work\\prog\\src.bin" 2>&1 | tr -d '\r' | grep '^# integrity')
    data_soft=$(qd_container_data "$zigsaw" data-soft) || return
    data_low=$(qd_container_data "$zigsaw" data-low) || return
    case $out in "# integrity medium" | "# integrity high") ;; *) echo "got \"$out\""; return 1 ;; esac
    [ "$data_low" != "$data_soft" ] && no_label "$data_soft" && low_label "$data_low"
}
data_relabelled() {
    qd_run z --name data-next-low $PROG sh -c 'echo x > "$HOME/../again.txt"' >/dev/null 2>&1 || return 1
    local next data_low data_soft
    next=$(qd_container_data "$zigsaw" data-next-low) || return
    data_low=$(qd_container_data "$zigsaw" data-low) || return
    data_soft=$(qd_container_data "$zigsaw" data-soft) || return
    [ "$next" != "$data_low" ] && low_label "$next" && [ ! -e "$data_soft\\again.txt" ]
}
data_held() {
    hold "$work\\prog" $PROG --name data-held --filesystem="$work\\prog" || return 1
    local out held_data soft_data
    held_data=$(qd_container_data "$zigsaw" data-held)
    out=$(qd_run z --name data-concurrent-soft --sandbox=soft $PROG sh -c true 2>&1)
    soft_data=$(qd_container_data "$zigsaw" data-concurrent-soft)
    release
    ! grep -q 'data folder stays labelled' <<<"$out" && [ "$held_data" != "$soft_data" ] && low_label "$held_data" && no_label "$soft_data"
}
check "low: its private container data and the programs it writes there stay low" data_low_written
check "soft: its own data stays separate from the low container's data" data_soft_written
check "low: the next container labels its own data and writes there" data_relabelled

data_first_soft() {
    local out
    out=$(qd_run z --sandbox=soft $PROG sh -c true 2>&1) || { echo "$out"; return 1; }
    ! grep -q 'error' <<<"$out" || { echo "$out"; return 1; }
}
check "a concurrent soft container has separate data and does not inherit a low container's label" data_held
check "soft: an app's first run says nothing of its data folder" data_first_soft
check "rm forgets the programs, which stay medium" rm_forgets

# --- What's changed outside the stores: `zigsaw grants` -------------------------

store_a="$work\\store-a"
store_b="$work\\store-b"
for s in "$store_a" "$store_b"; do
    mkdir -p "$s\\cache\\downloads"
    cp -l "$(cygpath -u "$QUASIDOCK_HOME")"/cache/downloads/* "$(cygpath -u "$s")/cache/downloads/" 2>/dev/null ||
        cp "$(cygpath -u "$QUASIDOCK_HOME")"/cache/downloads/* "$(cygpath -u "$s")/cache/downloads/"
done
za() { QUASIDOCK_HOME="$store_a" "$zigsaw" "$@" </dev/null; }
zb() { QUASIDOCK_HOME="$store_b" "$zigsaw" "$@" </dev/null; }
LEDGER=test.sandbox.ledger
sed -e "s/\"busybox\"/\"$LEDGER\"/" "$root/recipes/busybox.json" >"$work\\ledger.json"
za build "$work\\ledger.json" >/dev/null 2>&1 && zb build "$work\\ledger.json" >/dev/null 2>&1 ||
    { echo "building the record's app failed"; exit 1; }
mkdir -p "$work\\both"

nothing_said() { za grants | grep -q 'changed nothing outside its stores'; }
check "grants: nothing changed, nothing to say" nothing_said
one_app() { za grants test.sandbox.nobody | grep -q 'Nothing recorded'; }
check "grants <app> says when nothing is recorded" one_app
takes_back_unexplained() {
    icacls "$work\\both" /setintegritylevel '(OI)(CI)L' >/dev/null || return 1
    zb grants --take-back "$work\\both" >/dev/null 2>&1 && no_label "$work\\both"
}
check "grants --take-back removes a low label" takes_back_unexplained
relative_refused() { ! zb grants --take-back 'relative\path' >/dev/null 2>&1; }
check "  and refuses a relative path" relative_refused
qd_remove_app za $LEDGER >/dev/null 2>&1
qd_remove_app zb $LEDGER >/dev/null 2>&1

rm -rf "$QUASIDOCK_HOME" "$work"
echo
echo "$failures check(s) failed."
exit $((failures > 0))
