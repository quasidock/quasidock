#!/usr/bin/env bash
# Signs the images published recipes make, as the registry has them now: each recipe's
# own version and "latest". Those with a good signature are left alone, so that it can
# be run again after it stops. The images themselves don't change: a signature goes
# beside its image, under a tag of its own, and no digest moves.
#
#   scripts/sign-images.sh [recipe.json...]    (default: those in published-recipes.txt)
#   scripts/sign-images.sh --images IMAGE...  sign exact candidate references
#
# COSIGN_KEY names the key: a cosign.key from scripts/signing-key.sh. Its passphrase is
# asked for once, unless COSIGN_PASSWORD has it. Older versions aren't signed: they're
# old for a reason, and pull warns about them.
#
# Needs Git Bash, a quasidock built with the matching public key (`go run ./scripts/build.go`),
# with a login for the registry that may push (`quasidock login ghcr.io`, or
# QUASIDOCK_REGISTRY_USERNAME and QUASIDOCK_REGISTRY_PASSWORD), and cosign in its store.
# The registry is the default one, or QUASIDOCK_REGISTRY's.

set -eu
export MSYS_NO_PATHCONV=1

root=$(cd "$(dirname "$0")/.." && pwd)
root_w=$(cygpath -w "$root")
quasidock=${QUASIDOCK:-$root_w\\out\\bin\\quasidock.exe}
# shellcheck source=scripts/sign-lib.sh
. "$root/scripts/sign-lib.sh"

check_key
cosign=$(find_cosign)
ask_passphrase

images=()
if [ "${1:-}" = --images ]; then
    shift
    [ $# -gt 0 ] || { echo 'usage: sign-images.sh --images IMAGE...' >&2; exit 2; }
    images=("$@")
    set --
elif [ $# -eq 0 ]; then
    for name in $(grep -v '^#' "$root/scripts/published-recipes.txt" | tr -d '\r'); do set -- "$@" "$root/recipes/$name"; done
fi
field() { grep -o "\"$1\": *\"[^\"]*\"" "$2" | head -1 | cut -d'"' -f4; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
work_w=$(cygpath -w "$work")

signed=0
already=0
for recipe in "$@"; do
    id=$(field id "$recipe")
    version=$(field version "$recipe")
    for tag in "$version" latest; do
        images+=("$id:$tag")
    done
done
for image in "${images[@]}"; do
        if "$quasidock" signature check "$image" >/dev/null 2>&1; then
            already=$((already + 1))
            continue
        fi
        "$quasidock" signature payload "$image" >"$work/payload" || { echo "error: $image isn't there to sign" >&2; exit 1; }
        sign_blob "$work_w\\payload" "$work_w\\signature"
        "$quasidock" signature push "$image" "$work_w\\payload" "$work_w\\signature"
        signed=$((signed + 1))
done
echo "$signed signed, $already signed already."
