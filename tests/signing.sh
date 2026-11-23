#!/usr/bin/env bash
# End-to-end checks for images' signatures: what pull, update and info do with a good
# one, a wrong one, and none, against a local registry, with keys `zigsaw trust` adds.
# Signatures are made by tests/cmd/p256sign, in the form cosign's are, which
# tests/oci.sh checks with the real thing. `upgrade`'s are in tests/install.sh.
#
#   tests/zot.sh bash tests/signing.sh [path\to\quasidock.exe]
#
# Takes less than a minute.

set -u
export MSYS_NO_PATHCONV=1
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"

root=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
quasidock=${1:-$root\\out\\bin\\quasidock.exe}
work=$(cygpath -wl "$(mktemp -d)")
mkdir -p "$work\\cwd" && cd "$work\\cwd" || exit 1
(cd "$root" && go run ./scripts/build.go p256sign resetproxy) || { echo "building the helpers failed"; exit 1; }
signer="$root\\out\\test\\quasidock-p256sign.exe"
proxy="$root\\out\\test\\quasidock-resetproxy.exe"

export QUASIDOCK_GRANTS_KEY="Software\\QuasidockTest\\signing-$$"
export DOCKER_CONFIG="$work\\docker"
mkdir -p "$DOCKER_CONFIG"
: "${AUTH_REGISTRY:?run this with tests/zot.sh}"

failures=0
check() {
    local label=$1
    shift
    local out
    if out=$("$@" 2>&1); then
        printf 'ok    %s\n' "$label"
    else
        printf 'FAIL  %s\n      %s\n' "$label" "$(grep -v '^\s*$' <<<"$out" | tail -1 | cut -c1-140)"
        failures=$((failures + 1))
    fi
}

registry=localhost:5000
namespace="quasidock-signing-$RANDOM"
repo="$registry/$namespace/app"
APP=test.signing.app

# The publisher's store, and those of people pulling.
za() { QUASIDOCK_HOME="$work\\publisher" "$quasidock" "$@"; }
in_store() { # <store name> <args>
    local name=$1
    shift
    QUASIDOCK_HOME="$work\\$name" "$quasidock" "$@"
}
blobs_in() { ls "$work\\$1\\blobs\\sha256" 2>/dev/null | wc -l; }
installed() { in_store "$1" inspect -f '{{.Quasidock.Version}}' "$APP" 2>/dev/null | tr -d '\r'; }

recipe() { # <version>
    printf '{ "id": "%s", "version": "%s", "entrypoint": "app.cmd", "permissions": { "sandbox": "soft" },\n  "stages": [{ "name": "scripts", "sources": [{ "path": "app.cmd" }] }] }\n' "$APP" "$1" >"$work\\app.json"
    printf '@echo version %s\r\n' "$1" >"$work\\app.cmd"
}
publish() { # <version> [<tag>]: the image's digest goes in $digest
    recipe "$1"
    za build "$work\\app.json" >/dev/null 2>&1 || return 1
    qd_push za "$APP" "$repo:${2:-$1}" >/dev/null 2>&1 || return 1
    digest=$(QUASIDOCK_HOME="$work\\publisher" qd_manifest "$quasidock" "$APP" | head -1)
}
"$signer" key one "$work\\one.pub"
"$signer" key other "$work\\other.pub"
sign_as() { # <word> <image>
    local p="$work\\payload-$RANDOM" s="$work\\sig-$RANDOM"
    za signature payload "$2" >"$p" || return 1
    "$signer" sign "$1" "$p" >"$s" || return 1
    za signature push "$2" "$p" "$s"
}
# Another's image, for keys in the right place and images in the wrong one.
fingerprint() { QUASIDOCK_HOME="$work\\fp" "$quasidock" trust add "example.test/fp-$1" "$work\\$1.pub" 2>&1 | grep -o 'key [0-9a-f]*' | cut -d' ' -f2; }
one_fp=$(fingerprint one)

# Copies a manifest, and what it refers to, to another repository and tag, with curl,
# as nothing in zigsaw would write one where it doesn't belong.
copy_manifest() { # <from repo> <from reference> <to repo> <to tag>
    local m="$work\\manifest-$RANDOM" d f loc
    curl -sf -H 'Accept: application/vnd.oci.image.manifest.v1+json' "http://$registry/v2/$1/manifests/$2" -o "$(cygpath -m "$m")" || return 1
    for d in $(grep -o 'sha256:[0-9a-f]\{64\}' "$m" | sort -u); do
        f="$work\\blob-$RANDOM"
        curl -sf "http://$registry/v2/$1/blobs/$d" -o "$(cygpath -m "$f")" || continue
        loc=$(curl -s -D - -o NUL -X POST "http://$registry/v2/$3/blobs/uploads/" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')
        case $loc in http*) ;; *) loc="http://$registry$loc" ;; esac
        curl -sf -o NUL -X PUT -H 'Content-Type: application/octet-stream' --data-binary "@$(cygpath -m "$f")" "$loc?digest=$d" || return 1
    done
    curl -sf -o NUL -X PUT -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' --data-binary "@$(cygpath -m "$m")" "http://$registry/v2/$3/manifests/$4"
}
sig_tag() { printf 'sha256-%s.sig' "${1#sha256:}"; }

publish 1 || { echo "publishing the fixture failed"; exit 1; }
digest1=$digest

# --- Keys -----------------------------------------------------------------------

added_keys() { in_store "$1" trust | tr -d '\r' | grep -v ', built in$'; }
built_in_only() {
    local keys
    keys=$(in_store plain trust | tr -d '\r') || return 1
    if grep -q -- '-----BEGIN PUBLIC KEY-----' "$root\\internal\\trust\\quasidock.pub"; then
        [ "$(grep -c . <<<"$keys")" = 1 ] &&
            grep -q '^ghcr.io/quasidock/quasidock  key [0-9a-f]*, built in$' <<<"$keys"
    else
        [ -z "$keys" ]
    fi
}
check "a store has only the configured Quasidock key, for its images only" built_in_only
key_refused() {
    printf 'not a key\n' >"$work\\notkey.pub"
    ! in_store trusting trust add "$registry/$namespace" "$work\\notkey.pub" 2>/dev/null
}
check "trust add refuses what isn't a P-256 public key" key_refused
key_added() {
    in_store trusting trust add "$registry/$namespace/" "$work\\one.pub" 2>/dev/null &&
        in_store trusting trust | grep -q "^$registry/$namespace  key $one_fp\$"
}
check "  and lists one it took, with the slash at the end gone" key_added
twice() { in_store trusting trust add "$registry/$namespace" "$work\\one.pub" 2>&1 | grep -q 'already has' && [ "$(added_keys trusting | wc -l)" = 1 ]; }
check "  once" twice

# --- Nothing to go by --------------------------------------------------------------

unkeyed() {
    local out
    out=$(in_store plain pull --yes "$repo:1" 2>&1) || { echo "$out"; return 1; }
    grep -q 'no key here for its registry' <<<"$out" || { echo "$out"; return 1; }
    [ "$(installed plain)" = 1 ]
}
check "with no key for its registry, an image installs, saying it wasn't checked" unkeyed
info_unkeyed() { in_store plain image info "$repo:1" 2>/dev/null | grep -q 'signature *not checked'; }
check "  and info says so" info_unkeyed

# --- Missing ---------------------------------------------------------------------

store_with_key() { # <name> [word]
    in_store "$1" trust add "$registry/$namespace" "$work\\${2:-one}.pub" >/dev/null 2>&1
}
missing_warns() {
    store_with_key m1
    local out
    out=$(in_store m1 pull --yes "$repo:1" 2>&1) || { echo "$out"; return 1; }
    grep -q "warning: .* isn't signed" <<<"$out" || { echo "$out"; return 1; }
    [ "$(installed m1)" = 1 ]
}
check "with a key and no signature, it warns and installs, for now" missing_warns
info_missing() { in_store m1 image info "$repo:1" 2>/dev/null | grep -q 'signature *none'; }
check "  and info says so" info_missing
check_missing() { ! in_store m1 signature check "$repo:1" 2>/dev/null; }
check "  and signature check fails" check_missing

# --- Good ------------------------------------------------------------------------

payload_names() {
    local p
    p=$(za signature payload "$repo:1") || return 1
    [ "$p" = "{\"critical\":{\"identity\":{\"docker-reference\":\"$repo\"},\"image\":{\"docker-manifest-digest\":\"$digest1\"},\"type\":\"cosign container image signature\"},\"optional\":null}" ] || { echo "$p"; return 1; }
}
check "signature payload says what cosign's does" payload_names
check "signature push puts one beside the image" sign_as one "$repo:1"
signature_tag() { curl -sf "http://$registry/v2/$namespace/app/tags/list" | grep -q "$(sig_tag "$digest1")"; }
check "  under the tag cosign uses" signature_tag
same_digest() {
    local out
    out=$(curl -s -H 'Accept: application/vnd.oci.image.manifest.v1+json' -o NUL -w '%{http_code}' "http://$registry/v2/$namespace/app/manifests/$digest1")
    [ "$out" = 200 ]
}
check "  and the image's digest is as it was" same_digest
good_pull() {
    store_with_key g1
    local out
    out=$(in_store g1 pull --yes "$repo:1" 2>&1) || { echo "$out"; return 1; }
    grep -q "signed, by key $one_fp" <<<"$out" || { echo "$out"; return 1; }
    ! grep -q 'warning' <<<"$out" || { echo "$out"; return 1; }
    [ "$(installed g1)" = 1 ]
}
check "pull installs an image with a good signature, and says whose" good_pull
good_info() { in_store g1 image info "$repo:1" 2>/dev/null | grep -q "signature *signed, by key $one_fp"; }
check "  and info says so" good_info
check "  and signature check passes" in_store g1 signature check "$repo:1"
by_digest() {
    store_with_key g2
    in_store g2 pull --yes "$repo@$digest1" 2>&1 | grep -q "signed, by key $one_fp"
}
check "  by digest too" by_digest
second_key() {
    sign_as other "$repo:1" >/dev/null 2>&1 || return 1
    store_with_key k2 other
    in_store k2 pull --yes "$repo:1" 2>&1 | grep -q 'signed, by key' || return 1
    store_with_key k1 one
    in_store k1 pull --yes "$repo:1" 2>&1 | grep -q "signed, by key $one_fp"
}
check "a second signature joins the first, and either key's good" second_key

# --- Wrong -------------------------------------------------------------------------

refused() { # <store> <what it should say>
    local out
    out=$(in_store "$1" pull --yes "$image" 2>&1) && { echo "it installed: $out"; return 1; }
    grep -q "$2" <<<"$out" || { echo "$out"; return 1; }
    grep -q 'Nothing was installed' <<<"$out" || { echo "$out"; return 1; }
    [ "$(blobs_in "$1")" = 0 ] || { echo "it downloaded $(blobs_in "$1") blob(s)"; return 1; }
    [ -z "$(installed "$1")" ]
}

publish 2 && digest2=$digest
image="$repo:2"
wrong_key() {
    sign_as other "$image" >/dev/null 2>&1 || return 1
    store_with_key w1 one
    refused w1 'not by any key that counts'
}
check "a signature by another key is refused, with nothing downloaded" wrong_key
wrong_info() { in_store w1 image info "$image" 2>/dev/null | grep -q 'signature *WRONG'; }
check "  and info says so" wrong_info

publish 3 && digest3=$digest
image="$repo:3"
other_image() {
    # A good signature of version 1, under the tag for version 3's.
    copy_manifest "$namespace/app" "$(sig_tag "$digest1")" "$namespace/app" "$(sig_tag "$digest3")" || { echo "copying failed"; return 1; }
    store_with_key w2 one
    refused w2 'of another image'
}
check "a good signature of another image is refused" other_image

other_repo="$namespace/another"
replayed() {
    # Version 1 as if it were another app's, its signature beside it.
    copy_manifest "$namespace/app" "$digest1" "$other_repo" 1 || { echo "copying the image failed"; return 1; }
    copy_manifest "$namespace/app" "$(sig_tag "$digest1")" "$other_repo" "$(sig_tag "$digest1")" || { echo "copying its signature failed"; return 1; }
    store_with_key w3 one
    image="$registry/$other_repo:1"
    refused w3 'in another repository'
}
check "a signed image served under another repository's name is refused" replayed

publish 4 && digest4=$digest
image="$repo:4"
tampered() {
    local p="$work\\tp" changed="$work\\tp2" s="$work\\ts"
    za signature payload "$image" >"$p" || return 1
    "$signer" sign one "$p" >"$s" || return 1
    # What's signed, changed: still about this image, as far as it says.
    sed 's/"optional":null/"optional":{"a":"b"}/' "$p" >"$changed"
    za signature push "$image" "$changed" "$s" >/dev/null 2>&1 || { echo "pushing failed"; return 1; }
    store_with_key w4 one
    refused w4 'not by any key that counts'
}
check "a payload that isn't what was signed is refused" tampered
refuses_other_payload() {
    local p="$work\\op" s="$work\\os"
    za signature payload "$repo:1" >"$p" && "$signer" sign one "$p" >"$s" || return 1
    ! za signature push "$registry/$other_repo:1" "$p" "$s" 2>/dev/null
}
check "signature push won't put a repository's payload beside another's image" refuses_other_payload

# --- Under a prefix --------------------------------------------------------------

sibling() {
    local out
    in_store p1 trust add "$registry/$namespace/ap" "$work\\one.pub" >/dev/null 2>&1
    out=$(in_store p1 pull --yes "$repo:1" 2>&1) || { echo "$out"; return 1; }
    grep -q 'no key here for its registry' <<<"$out"
}
check "a key is for whole path components: .../ap doesn't reach .../app" sibling
removed() {
    in_store w1 trust rm "$registry/$namespace" >/dev/null 2>&1 || return 1
    [ -z "$(added_keys w1)" ] || return 1
    in_store w1 pull --yes "$repo:2" 2>&1 | grep -q 'no key here for its registry'
}
check "trust rm takes a key away, and the image it refused now goes unchecked" removed
rm_nothing() { ! in_store plain trust rm "$registry/$namespace" 2>/dev/null; }
check "  and it says when there was nothing to take" rm_nothing

# --- What a build pins ----------------------------------------------------------------

pinned() {
    # A digest pin fixes the bytes; the registry's trusted signing key still
    # authenticates their publisher when the dependency is fetched.
    store_with_key b1 one
    printf '{ "id": "test.signing.user", "version": "1", "entrypoint": "use.cmd", "permissions": { "sandbox": "soft" },\n  "from": { "base": "%s:2@%s" },\n  "stages": [{ "name": "scripts", "sources": [{ "path": "use.cmd" }] }] }\n' "$repo" "$digest2" >"$work\\use.json"
    printf '@echo used\r\n' >"$work\\use.cmd"
    local out
    out=$(in_store b1 build "$work\\use.json" 2>&1) && { echo "accepted the wrong signature"; return 1; }
    grep -q "it's signed, but" <<<"$out" || { echo "$out"; return 1; }
    sign_as one "$repo:2" >/dev/null 2>&1 && in_store b1 build "$work\\use.json"
}
check "a build verifies signatures on pinned dependencies" pinned

# --- A registry having a bad day -----------------------------------------------------

proxy_port=$((20000 + RANDOM % 20000))
"$proxy" "$proxy_port" 5000 .sig 0 --answer-500 &
proxy_pid=$!
sleep 1
bad_day() {
    in_store e1 trust add "localhost:$proxy_port/$namespace" "$work\\one.pub" >/dev/null 2>&1
    local out
    out=$(in_store e1 pull --yes "localhost:$proxy_port/$namespace/app:1" 2>&1) && { echo "it installed: $out"; return 1; }
    ! grep -q "isn't signed" <<<"$out" || { echo "it took that for unsigned: $out"; return 1; }
    grep -q '500' <<<"$out" || { echo "$out"; return 1; }
    [ -z "$(installed e1)" ]
}
check "a registry that fails at the signature is an error, not an unsigned image" bad_day
kill "$proxy_pid" 2>/dev/null

# --- An index -------------------------------------------------------------------------

indexed() {
    # An index of version 1, whose own digest is what's signed.
    local i="$work\\index.json" size index p="$work\\ip" s="$work\\is"
    size=$(curl -s -H 'Accept: application/vnd.oci.image.manifest.v1+json' "http://$registry/v2/$namespace/app/manifests/$digest1" | wc -c)
    printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"os":"windows","architecture":"amd64"}}]}' "$digest1" "$size" >"$i"
    index=sha256:$(sha256sum <"$i" | cut -d' ' -f1)
    copy_manifest "$namespace/app" "$digest1" "$namespace/indexed" one || return 1
    curl -sf -o NUL -X PUT -H 'Content-Type: application/vnd.oci.image.index.v1+json' --data-binary "@$(cygpath -m "$i")" "http://$registry/v2/$namespace/indexed/manifests/idx" || { echo "pushing the index failed"; return 1; }
    printf '{"critical":{"identity":{"docker-reference":"%s"},"image":{"docker-manifest-digest":"%s"},"type":"cosign container image signature"},"optional":null}' "$registry/$namespace/indexed" "$index" >"$p"
    "$signer" sign one "$p" >"$s" || return 1
    za signature push "$registry/$namespace/indexed:idx" "$p" "$s" >/dev/null 2>&1 || { echo "pushing the signature failed"; return 1; }
    store_with_key x1 one
    local out
    out=$(in_store x1 pull --yes "$registry/$namespace/indexed:idx" 2>&1) || { echo "$out"; return 1; }
    grep -q "signed, by key $one_fp" <<<"$out" || { echo "$out"; return 1; }
}
check "a signature of a multi-platform index covers its image" indexed

for s in "$work"/*/; do QUASIDOCK_HOME="$(cygpath -w "$s")" qd_remove_app "$quasidock" "$APP" >/dev/null 2>&1; done
reg delete "HKCU\\$QUASIDOCK_GRANTS_KEY" /f >/dev/null 2>&1
reg query 'HKCU\Software\QuasidockTest' 2>/dev/null | grep -q . || reg delete 'HKCU\Software\QuasidockTest' /f >/dev/null 2>&1
cd / && rm -rf "$work" 2>/dev/null

echo
echo "$failures check(s) failed."
exit $((failures > 0))
