#!/usr/bin/env bash
# GTK 3 end to end: GTK 3's images are an add-on to GTK 4's. An app built
# against GTK 3's SDK image (gtk3-sdk) and GTK 4's, with Meson and
# llvm-mingw's Clang, runs on both runtimes (gtk3 and gtk4),
# opens a window and draws it in each sandbox, with GTK 3's settings schemas
# and the legacy icon theme from its runtime; GTK 3's demos (gtk3demo)
# run, with the windowless shim and Start menu shortcuts (in a temporary
# folder). Gnumeric (recipes/gnumeric.json) converts a spreadsheet between
# CSV, Excel and OpenDocument with ssconvert in each sandbox, draws charts,
# writes into a folder of the user's it's granted, starts up with a file given
# with --file, and is registered for its kinds of file (in a scratch registry
# key). Meld (recipes/meld.json), Python on PyGObject's runtime, loads GTK 3,
# GtkSourceView and GTK 4 through their typelibs, and compares files and
# folders in each sandbox, and shows a git repository's changes with the git
# of MinGit's image, one of its runtimes. Last, the published apps in the
# store, GTK 4's too, are all on the same build of each GTK runtime.
#
# Where Windows denies an AppContainer its own executable's real path
# (tests/acprobe.sh: self-path-dos, as on Server 2025), CPython's getpath
# warns that it can't find it, and goes on: the Python checks leave that
# line out there. Where it denies NUL too (nul, as on Server 2025), Meld
# can't open os.devnull for its output, and git can't open /dev/null: Meld's
# window and its git support under AppContainer are known gaps there. With
# LOG_DIR set, a failed draw leaves the run's output and Meld's log there.
#
# In CI, as tests/gtk.sh, in a store of the published images, which it
# installs again rather than building (GTK's SDK takes a quarter of an hour
# to build, and Gnumeric 25 minutes).
#
#   GTK_HOME=path\to\store bash tests/gtk3.sh [path\to\quasidock.exe]
#
# The store needs GTK 4's SDK and runtime (GTK_HOME, or BUILD_HOME as
# tests/published.sh leaves it); GTK 3's images, the demos, Gnumeric,
# PyGObject's images and Meld are built there unless it has them (5 minutes,
# and Gnumeric 25), and removed afterwards if they were. The apps open
# windows on the desktop for a moment.
set -u
export MSYS_NO_PATHCONV=1
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"

root=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
quasidock=${1:-$root/out/bin/quasidock.exe}
QUASIDOCK_HOME=${GTK_HOME:-${BUILD_HOME:-}}
if [ -z "$QUASIDOCK_HOME" ] || ! qd_cached "$quasidock" "gtk4" || ! qd_cached "$quasidock" "gtk4-sdk"; then
    echo "tests/gtk3.sh needs a store with gtk4 and gtk4-sdk in GTK_HOME or BUILD_HOME;"
    echo "build recipes\\gtk4-sdk.json and recipes\\gtk4.json into one first."
    exit 1
fi
export QUASIDOCK_HOME
work=$(cygpath -w "$(mktemp -d)")
APP=test.gtk.hello3
DEMO=gtk3demo
GNUMERIC=gnumeric
mkdir -p "$work\\folders\\Documents"

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

# check_want <ok|gap> <label> <command...>: check, but a failure where the
# probe says a known gap's causes are there counts as that gap.
gaps=0
check_want() {
    local want=$1 label=$2
    shift 2
    [ "$want" = gap ] || { check "$label" "$@"; return; }
    local out
    if out=$("$@" 2>&1); then
        printf 'ok    %s  (a known gap that passes now)\n' "$label"
    else
        printf 'gap   %s  (known gap)\n      %s\n' "$label" "$(grep -v '^\s*$' <<<"$out" | tail -1 | cut -c1-110)"
        gaps=$((gaps + 1))
    fi
}

z() { "$quasidock" "$@"; }
manifest_of() { qd_manifest "$quasidock" "$1"; }
# install_again <id> <recipe>: installs an app the store has again, as the
# store got it: from the build cache, if it built it, or the registry it was
# pulled from (as the Reproduce workflow's store pulls the published
# images), so its shortcuts and file associations go to this test's folder
# and registry key. Builds the recipe if the store hasn't the app.
install_again() {
    local source=""
    qd_cached "$quasidock" "$1" && source=$(qd_source "$quasidock" "$1")
    case $source in
    "" | *.json) z build "$root\\recipes\\$2" ;;
    *) z pull --yes "$source" ;;
    esac
}
# build_image <id> <recipe>: builds the recipe, or installs the image again
# if the store has it (see install_again); notes whether it was installed
# before (for removing what this installed at the end).
installed_here=""
build_image() {
    qd_cached "$quasidock" "$1" || installed_here="$installed_here $1"
    install_again "$1" "$2" >"$work\\$2.log" 2>&1 || echo "installing $2 failed: $(tail -1 "$work\\$2.log")"
}
build_image gtk3-sdk gtk3-sdk.json
build_image gtk3 gtk3.json
build_image $DEMO gtk3demo.json
. "$(cygpath -u "$root")/tests/acprobe.sh"
acprobe_run "$quasidock" "$root" "$work"

cp "$root\\tests\\gtk\\gtk3\\hello3.c" "$root\\tests\\gtk\\gtk3\\meson.build" "$work\\"
sed -e "s/@GTK@/$(manifest_of gtk4)/" -e "s/@GTKSDK@/$(manifest_of gtk4-sdk)/" \
    -e "s/@GTK3@/$(manifest_of gtk3)/" -e "s/@GTK3SDK@/$(manifest_of gtk3-sdk)/" \
    "$root\\tests\\gtk\\gtk3\\hello3.json.in" >"$work\\hello3.json"
check "an app builds against GTK 3's SDK image and GTK 4's, finding gtk+-3.0 with pkg-config" z build "$work\\hello3.json"
# Runs from the work directory: the app has the cwd permission.
cd "$work" || exit 1
drew() {
    local out
    out=$(timeout 60 bash "$QD_IMAGES_HELPER" run "$quasidock" "$@" $APP 2>&1 | tr -d '\r')
    grep -q '^GTK 3\.24\.52$' <<<"$out" && grep -q '^ATK 2\.60\.7$' <<<"$out" &&
        grep -q '^FileChooser schema found$' <<<"$out" && grep -q '^icon document-open found$' <<<"$out" &&
        grep -q '^loaded it at 48x48$' <<<"$out" && grep -q '^drew a frame$' <<<"$out" || { echo "got: $(tr '\n' '|' <<<"$out")"; return 1; }
}
check "it runs on both runtimes, with GTK 3's schemas and legacy icons, and draws its window" drew
check "with --sandbox=soft" drew --sandbox=soft
check "with --sandbox=appcontainer" drew --sandbox=appcontainer
# GTK 4's runtime's variables, which GTK 3's apps have too.
check "GLib keeps settings in a key file in the app's data" sh -c "timeout 60 bash '$QD_IMAGES_HELPER' run \"\$0\" $APP 2>&1 | grep -q '^settings in GKeyfileSettingsBackend'" "$quasidock"
runtime_dir="$QUASIDOCK_HOME\\deploy\\$(grep -o 'sha256:[0-9a-f]*' "$QUASIDOCK_HOME\\blobs\\sha256\\$(manifest_of gtk3 | cut -d: -f2)" | tail -1 | cut -d: -f2)"
runtime_files() {
    test -f "$runtime_dir\\bin\\libgtk-3-0.dll" -a -f "$runtime_dir\\bin\\libatk-1.0-0.dll" || return 1
    ! test -e "$runtime_dir\\include" && ! test -e "$runtime_dir\\lib\\pkgconfig" && ! test -e "$runtime_dir\\bin\\libglib-2.0-0.dll" || return 1
    ! ls "$runtime_dir\\lib" | grep -q -e '\.a$' -e '\.lib$'
}
check "GTK 3's runtime has GTK 3 and ATK only: no headers, libraries for linking, or GLib" runtime_files
check "and their typelibs" test -f "$runtime_dir\\lib\\girepository-1.0\\Gtk-3.0.typelib" -a -f "$runtime_dir\\lib\\girepository-1.0\\Gdk-3.0.typelib" -a -f "$runtime_dir\\lib\\girepository-1.0\\Atk-1.0.typelib"
check "it has GTK 3's translations" test -f "$runtime_dir\\share\\locale\\de\\LC_MESSAGES\\gtk30.mo"

# stays_up <seconds> <quasidock run arguments...>: the app is still running
# when the time is up, rather than having crashed or failed a check; a
# critical warning stops it.
stays_up() {
    local seconds=$1
    shift
    timeout "$seconds" bash "$QD_IMAGES_HELPER" run "$quasidock" --env=G_DEBUG=fatal-criticals "$@" >/dev/null 2>&1
    [ $? -eq 124 ]
}
check "GTK 3's demo runs" stays_up 10 $DEMO
check "its widget factory runs, without critical warnings for want of a Desktop" stays_up 10 --entrypoint=gtk3-widget-factory $DEMO
check "with --sandbox=soft" stays_up 10 --sandbox=soft --entrypoint=gtk3-widget-factory $DEMO
check "with --sandbox=appcontainer" stays_up 10 --sandbox=appcontainer --entrypoint=gtk3-widget-factory $DEMO

# Gnumeric, on both runtimes. Built here unless the store has it, which
# takes about 25 minutes.
build_image $GNUMERIC gnumeric.json
gnumeric_dir="$QUASIDOCK_HOME\\deploy\\$(grep -o 'sha256:[0-9a-f]*' "$QUASIDOCK_HOME\\blobs\\sha256\\$(manifest_of $GNUMERIC | cut -d: -f2)" | tail -1 | cut -d: -f2)"
check "Gnumeric 1.12.62 is installed" sh -c "timeout 60 bash '$QD_IMAGES_HELPER' run \"\$0\" --entrypoint=ssconvert $GNUMERIC --version | tr -d '\r' | grep -qx \"ssconvert version '1.12.62'\"" "$quasidock"
check "it has its translations, goffice's and libgsf's" test -f "$gnumeric_dir\\share\\locale\\de\\LC_MESSAGES\\gnumeric-1.12.62.mo" -a -f "$gnumeric_dir\\share\\locale\\de\\LC_MESSAGES\\goffice-0.10.62.mo" -a -f "$gnumeric_dir\\share\\locale\\de\\LC_MESSAGES\\libgsf.mo"
# A CSV with a formula, to Excel, to OpenDocument and back, as ssconvert's
# plugins (Excel's, OpenDocument's, and the text importer) read and write
# them; the formula is computed.
printf 'item,count,double\nnails,21,=B2*2\n' >"$work\\sheet.csv"
round_trip() {
    rm -f "$work\\sheet.xlsx" "$work\\sheet.ods" "$work\\back.csv"
    local ss=(timeout 60 bash "$QD_IMAGES_HELPER" run "$quasidock" --env=G_DEBUG=fatal-criticals "$@" --entrypoint=ssconvert $GNUMERIC)
    "${ss[@]}" sheet.csv sheet.xlsx >/dev/null 2>&1 && "${ss[@]}" sheet.xlsx sheet.ods >/dev/null 2>&1 &&
        "${ss[@]}" sheet.ods back.csv >/dev/null 2>&1 || return 1
    [ "$(tr -d '\r' <"$work\\back.csv" | tr '\n' '|')" = 'item,count,double|nails,21,42|' ] || { echo "got: $(cat "$work\\back.csv")"; return 1; }
}
check "ssconvert takes a spreadsheet from CSV to Excel to OpenDocument and back" round_trip
check "with --sandbox=soft" round_trip --sandbox=soft
check "with --sandbox=appcontainer" round_trip --sandbox=appcontainer
# A line chart and a pie chart, drawn by goffice's plot plugins, which
# ssconvert exports as PNGs.
cp "$root\\tests\\gtk\\gtk3\\charts.gnumeric" "$work\\"
charts() {
    rm -f "$work"\\chart.png.*
    timeout 60 bash "$QD_IMAGES_HELPER" run "$quasidock" --env=G_DEBUG=fatal-criticals "$@" --entrypoint=ssconvert $GNUMERIC --export-graphs charts.gnumeric chart.png >/dev/null 2>&1 || return 1
    local n
    for n in 0 1; do
        head -c 8 "$work\\chart.png.$n" | od -A n -t x1 | tr -d ' \n' | grep -qx '89504e470d0a1a0a' || { echo "chart.png.$n isn't a PNG"; return 1; }
    done
}
check "it draws a spreadsheet's charts as PNGs" charts --sandbox=low
check "with --sandbox=appcontainer" charts --sandbox=appcontainer
to_granted_folder() {
    rm -f "$work\\folders\\Documents\\sheet.xlsx"
    timeout 60 bash "$QD_IMAGES_HELPER" run "$quasidock" --sandbox=low --filesystem="$work\\folders\\Documents" --entrypoint=ssconvert $GNUMERIC sheet.csv "$work\\folders\\Documents\\sheet.xlsx" >/dev/null 2>&1 &&
        test -s "$work\\folders\\Documents\\sheet.xlsx"
}
check "at low integrity, it saves into a path granted with --filesystem" to_granted_folder
check "Gnumeric opens a spreadsheet from a granted folder" stays_up 20 --filesystem="$work\\folders\\Documents" --entrypoint=gnumeric $GNUMERIC "$work\\folders\\Documents\\sheet.xlsx"
check "with --sandbox=soft" stays_up 20 --sandbox=soft --filesystem="$work\\folders\\Documents" --entrypoint=gnumeric $GNUMERIC "$work\\folders\\Documents\\sheet.xlsx"
check "with --sandbox=appcontainer" stays_up 20 --sandbox=appcontainer --filesystem="$work\\folders\\Documents" --entrypoint=gnumeric $GNUMERIC "$work\\folders\\Documents\\sheet.xlsx"

# Meld, a Python app: PyGObject's runtime runs it, with GTK 3 and GTK 4
# through their runtimes' typelibs. Built here unless the store has them
# (about 3 minutes).
MELD=meld
build_image pygobject-sdk pygobject-sdk.json
build_image pygobject pygobject.json
build_image $MELD meld.json
pygobject_dir="$QUASIDOCK_HOME\\deploy\\$(grep -o 'sha256:[0-9a-f]*' "$QUASIDOCK_HOME\\blobs\\sha256\\$(manifest_of pygobject | cut -d: -f2)" | tail -1 | cut -d: -f2)"
printf '%s\n' 'import gi' 'gi.require_version("Gtk", "3.0")' 'gi.require_version("GtkSource", "4")' \
    'from gi.repository import Gtk, GtkSource, Pango' \
    'print("GTK", Gtk.get_major_version(), Gtk.get_minor_version(), Gtk.get_micro_version())' \
    'print("GtkSourceView", GtkSource.LanguageManager.get_default().get_language("python3").get_name())' \
    'print("Pango", Pango.version_string())' >"$work\\gi3.py"
printf '%s\n' 'import gi, cairo' 'gi.require_version("Gtk", "4.0")' 'gi.require_version("Adw", "1")' \
    'from gi.repository import Gtk, Adw' \
    'print("GTK", Gtk.get_major_version(), Gtk.get_minor_version(), Gtk.get_micro_version())' \
    'print("libadwaita", Adw.get_major_version(), Adw.get_minor_version())' \
    'print("pycairo", cairo.version)' >"$work\\gi4.py"
# python_says <script> <expected, lines joined by |> <run options...>:
# what the script prints, without quasidock's notes of what it granted and took
# back, and GLib's warnings (in an AppContainer, Pango's that DirectWrite
# can't open per-user fonts).
python_says() {
    local script=$1 want=$2 out getpath='^$'
    shift 2
    case " $* " in *" --sandbox=appcontainer "*)
        [ "$(ac_if self-path-dos)" = gap ] && getpath='^Failed to find real location of ' ;;
    esac
    out=$(timeout 60 bash "$QD_IMAGES_HELPER" run "$quasidock" "$@" --entrypoint="$pygobject_dir\\python.exe" $MELD "$script" 2>&1 | tr -d '\r' |
        grep -v -e '^granted ' -e '^labelled ' -e '^revoked ' -e '^removed the low integrity label ' -e '^\*\* (' -e '^$' -e "$getpath" | tr '\n' '|')
    [ "$out" = "$want|" ] || { echo "got: $out"; return 1; }
}
check "PyGObject loads GTK 3 and GtkSourceView 4 through their typelibs" python_says gi3.py 'GTK 3 24 52|GtkSourceView Python|Pango 1.58.2'
check "and GTK 4 and libadwaita, with pycairo" python_says gi4.py 'GTK 4 24 1|libadwaita 1 10|pycairo 1.29.2'
check "with --sandbox=soft" python_says gi3.py 'GTK 3 24 52|GtkSourceView Python|Pango 1.58.2' --sandbox=soft
check "with --sandbox=appcontainer" python_says gi3.py 'GTK 3 24 52|GtkSourceView Python|Pango 1.58.2' --sandbox=appcontainer
meld_dir="$QUASIDOCK_HOME\\deploy\\$(grep -o 'sha256:[0-9a-f]*' "$QUASIDOCK_HOME\\blobs\\sha256\\$(manifest_of $MELD | cut -d: -f2)" | tail -1 | cut -d: -f2)"
# Meld's command is pythonw.exe, which shows nothing: python.exe runs it here.
check "Meld 3.24.1 is installed" sh -c "timeout 60 bash '$QD_IMAGES_HELPER' run \"\$0\" --entrypoint='$pygobject_dir\\python.exe' $MELD '$meld_dir\\bin\\meld' --version 2>&1 | tr -d '\r' | grep -qx 'meld 3.24.1'" "$quasidock"
rm -f "$QUASIDOCK_HOME\\data\\$MELD\\share\\meld.log"
printf 'one\ntwo\nthree\n' >"$work\\left.txt"
printf 'one\n2\nthree\nfour\n' >"$work\\right.txt"
mkdir -p "$work\\left" "$work\\right"
cp "$work\\left.txt" "$work\\left\\same.txt"; cp "$work\\left.txt" "$work\\right\\same.txt"
cp "$work\\left.txt" "$work\\left\\changed.txt"; cp "$work\\right.txt" "$work\\right\\changed.txt"
# meld_draws <what to compare...> -- <run options...>: Meld opens the
# comparison and saves a picture of its window, which its CI hook does
# after 3 s before quitting. With LOG_DIR set, a failure leaves the run's
# output and Meld's log there, as meld-<n>.txt and meld-<n>.log.
draws=0
meld_draws() {
    local what=() opts=()
    while [ $# -gt 0 ] && [ "$1" != -- ]; do what+=("$1"); shift; done
    [ $# -gt 0 ] && shift
    opts=("$@")
    draws=$((draws + 1))
    rm -f "$work\\meld.png"
    timeout 90 bash "$QD_IMAGES_HELPER" run "$quasidock" --env=MELD_SCREENSHOT_AND_EXIT="$work\\meld.png" "${opts[@]}" $MELD "${what[@]}" >"$work\\meld-run.txt" 2>&1
    head -c 8 "$work\\meld.png" 2>/dev/null | od -A n -t x1 | tr -d ' \n' | grep -qx '89504e470d0a1a0a' || { meld_logs; echo "no picture"; return 1; }
    [ "$(wc -c <"$work\\meld.png")" -gt 20000 ] || { meld_logs; echo "a picture of $(wc -c <"$work\\meld.png") bytes"; return 1; }
}
meld_logs() {
    [ -n "${LOG_DIR:-}" ] || return 0
    { echo "quasidock run ${opts[*]} $MELD ${what[*]}"; cat "$work\\meld-run.txt"; } >"$LOG_DIR\\meld-$draws.txt"
    cp "$QUASIDOCK_HOME\\data\\$MELD\\share\\meld.log" "$LOG_DIR\\meld-$draws.log" 2>/dev/null
}
check "Meld compares two files and draws its window" meld_draws left.txt right.txt
check "with --sandbox=soft" meld_draws left.txt right.txt -- --sandbox=soft
check_want "$(ac_if nul)" "with --sandbox=appcontainer" meld_draws left.txt right.txt -- --sandbox=appcontainer
check "it compares two folders" meld_draws left right -- --sandbox=low
# Meld's version control views run git, which MinGit's image, one of Meld's
# runtimes, puts on its PATH: here it makes a repository, with a commit, a
# changed file and a new one, in a run of Meld.
git_in_meld() { (cd "$work\\repo" && timeout 60 bash "$QD_IMAGES_HELPER" run "$quasidock" --entrypoint=git $MELD -c user.name=t -c user.email=t@example.com "$@" 2>&1); }
make_repo() {
    rm -rf "$work\\repo" && mkdir "$work\\repo" || return 1
    git_in_meld init -q -b main && printf 'one\n' >"$work\\repo\\a.txt" && git_in_meld add a.txt && git_in_meld commit -q -m a || return 1
    printf 'two\n' >>"$work\\repo\\a.txt"
    printf 'new\n' >"$work\\repo\\b.txt"
}
check "MinGit's runtime gives Meld git, which makes a repository" make_repo
# Meld's own git support, on the repository in the run's working directory.
printf '%s\n' 'import os, gi' 'gi.require_version("Gtk", "3.0")' \
    'from meld.vc import get_vcs' 'from meld.vc.git import Vc' 'repo = os.path.abspath("repo")' \
    'print("found:", [cls.NAME for cls, enabled in get_vcs(repo) if enabled])' \
    'vc = Vc(repo)' 'vc.refresh_vc_state()' \
    'for e in sorted(vc.get_entries(repo), key=lambda e: e.name): print(e.name, e.get_status())' >"$work\\vc.py"
vc_says='found: ['"'"'Git'"'"']|a.txt Modified|b.txt Unversioned'
check "Meld's git support finds the repository, and what's changed in it" python_says vc.py "$vc_says"
check "with --sandbox=soft" python_says vc.py "$vc_says" --sandbox=soft
check_want "$(ac_if nul)" "with --sandbox=appcontainer" python_says vc.py "$vc_says" --sandbox=appcontainer
check "it shows the repository's changes in its window" meld_draws repo -- --sandbox=low
check "its log has no Python traceback" sh -c "! grep -q Traceback '$QUASIDOCK_HOME\\data\\$MELD\\share\\meld.log'"

# The published apps in the store, GTK 4's included if they're there, are on
# the same builds of GTK's runtimes: each has the installed runtime's layer
# (a runtime has one) among its own, so a store with all of them deploys
# each runtime once.
layers_of() { grep -o '"digest": *"sha256:[0-9a-f]*"' "$QUASIDOCK_HOME\\blobs\\sha256\\$(manifest_of "$1" | cut -d: -f2)" | tail -n +2 | grep -o 'sha256:[0-9a-f]*'; }
published_ids=$(for name in $(grep -v '^#' "$root\\scripts\\published-recipes.txt" | tr -d '\r'); do
    grep -o '"id": *"[^"]*"' "$root\\recipes\\$name" | head -1 | cut -d'"' -f4
done)
one_build() {
    local runtime layer id on=0 others=""
    for runtime in gtk4 gtk3; do
        layer=$(layers_of $runtime)
        for id in $(z inspect -f "{{range .Quasidock.From}}{{if eq .ID \"$runtime\"}}{{$.Quasidock.ID}}{{end}}{{end}}" $(z images -aq) | tr -d '\r'); do
            grep -qxF "$id" <<<"$published_ids" || continue
            on=$((on + 1))
            layers_of "$id" | grep -qxF "$layer" || others="$others $id"
        done
    done
    [ -z "$others" ] || { echo "on other builds of the runtimes:$others"; return 1; }
    echo "$on app(s)"
}
check "the published apps here are all on the same build of each GTK runtime" one_build

cd "$root" || exit 1
rm -rf "$docs"
for id in $installed_here; do qd_remove_app z "$id" >/dev/null 2>&1; done
qd_remove_app z $APP >/dev/null 2>&1
acprobe_rm "$quasidock"
rm -rf "$work"
echo
acprobe_report
echo
echo "$failures check(s) failed, and $gaps known gap(s)."
exit $((failures > 0))
