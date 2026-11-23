#!/usr/bin/env bash
# Refresh local recipe pins from their existing version tags on the registry.
#
#   bash scripts/repin.sh [--dry-run] [recipe.json...]
#
# Defaults to scripts/published-recipes.txt. Explicit files can also be named
# relative to recipes/. Uses check-pins.sh's anonymous registry lookup and
# QUASIDOCK_REGISTRY (default ghcr.io/quasidock/quasidock), without a local store.
# Only short id:version@sha256:... references change; versions, source hashes,
# fully qualified references and digest-only source mirrors stay as written.
# Every tag must resolve to a valid digest before any recipe is edited.
# Needs Git Bash (or bash), curl and GNU sed. For tests, REPIN_RECIPES and
# REPIN_PUBLISHED replace the recipes directory and published list.

set -euo pipefail
export LC_ALL=C
export MSYS_NO_PATHCONV=1

usage() { echo 'usage: repin.sh [--dry-run] [recipe.json...]'; }
die() { echo "error: $*" >&2; exit 1; }
dry_run=false
files=()
while [ $# -gt 0 ]; do
    case $1 in
    --dry-run) dry_run=true ;;
    --help|-h) usage; exit 0 ;;
    --) shift; files+=("$@"); break ;;
    -*) usage >&2; exit 2 ;;
    *) files+=("$1") ;;
    esac
    shift
done

root=$(cd "$(dirname "$0")/.." && pwd)
recipes=${REPIN_RECIPES:-$root/recipes}
published=${REPIN_PUBLISHED:-$root/scripts/published-recipes.txt}
if [ ${#files[@]} -eq 0 ]; then
    while IFS= read -r name || [ -n "$name" ]; do
        name=${name%$'\r'}
        case $name in ''|'#'*) continue ;; esac
        files+=("$recipes/$name")
    done <"$published"
fi

declare -A seen
selected=()
for file in "${files[@]}"; do
    if [ ! -f "$file" ] && [ -f "$recipes/$file" ]; then file=$recipes/$file; fi
    [ -f "$file" ] && [ -r "$file" ] || die "cannot read recipe $file"
    file=$(cd "$(dirname "$file")" && printf '%s/%s\n' "$PWD" "${file##*/}")
    [ -z "${seen[$file]:-}" ] || continue
    seen[$file]=1
    selected+=("$file")
done

work=$(mktemp -d)
trap 'rm -f "$work"/*; rmdir "$work"' EXIT
# Match the short references used by the publishing workflow, with valid tag
# characters so neither shell words nor sed expressions can contain operators.
pin_re='"[A-Za-z0-9_.-]+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}@sha256:[0-9a-f]{64}"'
: >"$work/pins"
for file in "${selected[@]}"; do
    { grep -oE "$pin_re" "$file" || true; } | tr -d '"' >>"$work/pins"
done
sed 's/@.*//; s/:/ /' "$work/pins" | sort -u >"$work/tags"
if ! xargs -r -P 8 -n 2 bash "$root/scripts/check-pins.sh" --tag <"$work/tags" >"$work/resolved"; then
    die 'registry lookup failed; no recipes changed'
fi
declare -A digest_of
while read -r id version digest; do
    digest_of[$id:$version]=$digest
done <"$work/resolved"
while read -r id version; do
    digest=${digest_of[$id:$version]:-}
    [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] ||
        die "cannot resolve $id:$version to a sha256 digest; no recipes changed"
done <"$work/tags"

changed=0
pins=0
i=0
for file in "${selected[@]}"; do
    : >"$work/edits"
    while IFS= read -r pin; do
        ref=${pin%%@*}
        digest=${digest_of[$ref]}
        [ "$pin" != "$ref@$digest" ] || continue
        echo "${file#"$root/"}: $pin -> $ref@$digest"
        # The only regular-expression metacharacter in these references is '.'.
        printf 's|"%s"|"%s@%s"|g\n' "${pin//./\\.}" "$ref" "$digest" >>"$work/edits"
        pins=$((pins + 1))
    done < <({ grep -oE "$pin_re" "$file" || true; } | tr -d '"' | sort -u)
    if [ -s "$work/edits" ]; then
        sed -f "$work/edits" "$file" >"$work/$i.new"
        changed=$((changed + 1))
    fi
    i=$((i + 1))
done

if ! $dry_run; then
    i=0
    for file in "${selected[@]}"; do
        if [ -f "$work/$i.new" ]; then cat "$work/$i.new" >"$file"; fi
        i=$((i + 1))
    done
fi
action=updated
$dry_run && action='would update'
echo "${#selected[@]} recipe(s): $action $pins pin(s) in $changed file(s)"
