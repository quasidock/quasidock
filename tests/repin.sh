#!/usr/bin/env bash
# Repin against a fake registry, without network, builds, or a local store.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
script=$root/scripts/repin.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/repin test.XXXXXXXX")
trap 'rm -f "$work"/*; rmdir "$work"' EXIT
export REPIN_RECIPES=$work
export REPIN_PUBLISHED=$work/published.txt
export REPIN_CALLS=$work/calls
export QUASIDOCK_REGISTRY=http://localhost:5000/test
export QUASIDOCK=$work/does-not-exist
export QUASIDOCK_HOME=$work/does-not-exist
old=$(printf 'a%.0s' {1..64})
export REPIN_NEW=sha256:$(printf 'b%.0s' {1..64})
export REPIN_BASE=sha256:$(printf 'c%.0s' {1..64})
export REPIN_OLD=sha256:$old
cat >"$work/app.json" <<EOF
{
  "id": "app", "version": "9",
  "sdk": {"tool": "tool.name:1.2-rc_1@sha256:$old"},
  "from": {"base": "base:2@sha256:$old"},
  "source_mirrors": ["ghcr.io/old/tool.name@sha256:$old"],
  "stages": [{"sources": [
    {"url": "https://example.org/source.zip", "sha256": "$old"},
    {"image": "ghcr.io/other/base:2@sha256:$old"},
    {"image": "toolXname:1Y2-rc_1@sha256:$old"},
    {"image": "current:3@sha256:$old"}
  ]}]
}
EOF
cat >"$work/unit.json" <<EOF
{"id":"unit","version":"1","builds":[{
  "sdk":{"tool":"tool.name:1.2-rc_1@sha256:$old"},
  "stages":[{"sources":[{"image":"base:2@sha256:$old"}]}]
}]}
EOF
printf '{"id":"no-pins","version":"1","sha256":"%s"}\n' "$old" >"$work/no-pins.json"
cp "$work/app.json" "$work/unselected.json"
printf '# published recipes\r\napp.json\r\n\r\nunit.json\r\nno-pins.json' >"$REPIN_PUBLISHED"
for name in app unit no-pins unselected; do cp "$work/$name.json" "$work/$name.before"; done

cat >"$work/curl" <<'EOF'
#!/usr/bin/env bash
for url; do :; done
printf '%s\n' "$url" >>"$REPIN_CALLS"
case $url in
http://localhost:5000/v2/test/tool.name/manifests/1.2-rc_1) digest=$REPIN_NEW ;;
http://localhost:5000/v2/test/base/manifests/2) digest=$REPIN_BASE ;;
http://localhost:5000/v2/test/toolxname/manifests/1Y2-rc_1|http://localhost:5000/v2/test/current/manifests/3) digest=$REPIN_OLD ;;
*) echo "unexpected registry lookup: $url" >&2; exit 9 ;;
esac
if [[ $url == */base/manifests/2 ]]; then
case ${REPIN_FAILURE:-} in
missing) printf 'HTTP/1.1 404 Not Found\r\n\r\n'; exit ;;
invalid) digest=sha256:not-a-digest ;;
network) exit 7 ;;
esac
fi
printf 'HTTP/1.1 200 OK\r\nDocker-Content-Digest: %s\r\n\r\n' "$digest"
EOF
chmod +x "$work/curl"
export PATH="$work:$PATH"
: >"$REPIN_CALLS"

bash "$script" --dry-run >"$work/output"
grep -q '3 recipe(s): would update 4 pin(s) in 2 file(s)' "$work/output"
for name in app unit no-pins unselected; do cmp "$work/$name.before" "$work/$name.json"; done
test "$(wc -l <"$REPIN_CALLS")" -eq 4
echo 'ok    dry run resolves each version once and leaves every file unchanged'

for failure in missing invalid network; do
    if REPIN_FAILURE=$failure bash "$script" >"$work/output" 2>"$work/errors"; then
        echo "FAIL  $failure registry response passed"
        exit 1
    fi
    grep -q 'no recipes changed' "$work/errors"
    for name in app unit no-pins unselected; do cmp "$work/$name.before" "$work/$name.json"; done
done
echo 'ok    missing tags, invalid digests, and network failures prevent all edits'

bash "$script" >"$work/output"
grep -q '3 recipe(s): updated 4 pin(s) in 2 file(s)' "$work/output"
# Compare complete bytes: only the two references should differ, including in
# build units. The tag punctuation must be literal when sed replaces a pin.
for name in app unit; do
    sed -e "s/tool\.name:1\.2-rc_1@sha256:$old/tool.name:1.2-rc_1@$REPIN_NEW/g" \
        -e "s|\"base:2@sha256:$old\"|\"base:2@$REPIN_BASE\"|g" \
        "$work/$name.before" >"$work/expected"
    cmp "$work/expected" "$work/$name.json"
done
for name in no-pins unselected; do cmp "$work/$name.before" "$work/$name.json"; done
echo 'ok    defaults refresh SDK, runtime, and build-unit image pins, preserving other bytes'

bash "$script" >"$work/output"
grep -q 'updated 0 pin(s) in 0 file(s)' "$work/output"
echo 'ok    a second run is unchanged'

cp "$work/app.before" "$work/app.json"
cp "$work/unit.before" "$work/unit.json"
bash "$script" app.json "$work/app.json" >"$work/output"
grep -q '1 recipe(s): updated 2 pin(s) in 1 file(s)' "$work/output"
cmp "$work/unit.before" "$work/unit.json"
echo 'ok    explicit paths and recipe names select only those files, once'

: >"$REPIN_CALLS"
bash "$script" "$work/no-pins.json" >"$work/output"
test ! -s "$REPIN_CALLS"
grep -q 'updated 0 pin(s) in 0 file(s)' "$work/output"
: >"$REPIN_PUBLISHED"
bash "$script" >"$work/output"
test ! -s "$REPIN_CALLS"
grep -q '0 recipe(s): updated 0 pin(s) in 0 file(s)' "$work/output"
echo 'ok    recipes without pins and empty published lists need no registry access'

for bad in --unknown "$work/missing.json"; do
    if bash "$script" "$bad" >"$work/output" 2>"$work/errors"; then exit 1; fi
    test ! -s "$REPIN_CALLS"
done
echo 'ok    invalid options and unreadable inputs fail before registry access'
