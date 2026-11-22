#!/usr/bin/env bash
# Smoke-test an exact staged reference using the published-suite conventions.
set -euo pipefail
export MSYS_NO_PATHCONV=1
root=$(cd "$(dirname "$0")/.." && pwd)
source "$root/scripts/images.sh"
cli=${QUASIDOCK:-$root/out/bin/quasidock.exe}
id=$1 image=$2
"$cli" pull "$image"
# Always run away from the checkout: low-integrity cwd grants belong to scratch.
scratch=$(mktemp -d)
trap 'rmdir "$scratch" 2>/dev/null || true' EXIT
cd "$scratch"
# These are files in the SDK images, so using one tests what downstream recipe
# commands will find on PATH after deployment (including runtime paths).
sdk_commands=()
case $id in
zig) sdk_commands=(cc ar ranlib) ;;
llvm-mingw) sdk_commands=(cc ar ranlib windres) ;;
rust) sdk_commands=(dlltool) ;;
maven) sdk_commands=(mvn) ;;
gradle) sdk_commands=(gradle) ;;
meson) sdk_commands=(meson ninja pkg-config) ;;
composer) sdk_commands=(composer) ;;
make) sdk_commands=(gmake) ;;
esac
for command in "${sdk_commands[@]}"; do
    timeout 120 "$cli" run --rm --entrypoint="sdk-bin/$command.exe" "$image" --version </dev/null
done
case " vscodium notepad-plus-plus sumatrapdf gtk3demo scenebuilder qt6 notes " in
*" $id "*)
    cid=$("$cli" run -d --rm "$image")
    trap '"$cli" rm -f "$cid" >/dev/null 2>&1 || true; rmdir "$scratch" 2>/dev/null || true' EXIT
    sleep 10
    test "$("$cli" container inspect -f '{{.State.Running}}' "$cid" | tr -d '\r')" = true
    exit
    ;;
esac
if [ "$id" = kanboard ]; then
    port=$((20000 + RANDOM % 20000))
    cid=$("$cli" run -d --rm --entrypoint=kanboard "$image" --no-browser --listen=127.0.0.1:$port)
    trap '"$cli" rm -f "$cid" >/dev/null 2>&1 || true; rmdir "$scratch" 2>/dev/null || true' EXIT
    for _ in $(seq 40); do
        if curl -fsS -m 5 "http://127.0.0.1:$port/?controller=AuthController&action=login" 2>/dev/null | grep -q 'name="csrf_token"'; then exit 0; fi
        sleep 0.5
    done
    "$cli" logs "$cid"
    exit 1
fi
case $id in
apache) args=(-v) ;;
rnote) args=(--help) ;;
busybox) args=(true) ;;
node) args=(-e 0) ;;
python|python-sdk) args=(-c pass) ;;
zig|go|pdfbox|oras|cosign) args=(version) ;;
7zip) args=(i) ;;
vlc) args=(-I dummy vlc://quit) ;;
*) args=(--version) ;;
esac
timeout 120 "$cli" run --rm --entrypoint="$id" "$image" "${args[@]}" </dev/null
