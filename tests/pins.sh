#!/usr/bin/env bash
# Registry pin checks, including an empty release, without network access.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
export PIN_TEST_DIGEST=sha256:$(printf 'a%.0s' {1..64})
cat >"$work/app.json" <<'EOF'
{"id": "app", "version": "1"}
EOF
cat >"$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
for url; do :; done
if [ "${PIN_TEST_EMPTY:-0}" = 1 ] || { [ "${PIN_TEST_UNSIGNED:-0}" = 1 ] && [[ $url == *.sig ]]; }; then
    printf 'HTTP/1.1 404 Not Found\r\n\r\n'
else
    printf 'HTTP/1.1 200 OK\r\nDocker-Content-Digest: %s\r\n\r\n' "$PIN_TEST_DIGEST"
fi
EOF
chmod +x "$work/bin/curl"
export PATH="$work/bin:$PATH"
export QUASIDOCK_REGISTRY=ghcr.io/quasidock/quasidock
if PIN_TEST_EMPTY=1 bash "$root/scripts/check-pins.sh" "$work/app.json" >"$work/output" 2>"$work/errors"; then
    echo 'FAIL  an empty registry passed'
    exit 1
fi
grep -q '1 missing, 0 older runtime(s), 0 unsigned' "$work/output"
test ! -s "$work/errors"
echo 'ok    empty registry fails cleanly without an empty signature lookup'
bash "$root/scripts/check-pins.sh" "$work/app.json" >"$work/output" 2>"$work/errors"
grep -q '0 missing, 0 older runtime(s), 0 unsigned' "$work/output"
test ! -s "$work/errors"
echo 'ok    a published image with its signature passes'
if PIN_TEST_UNSIGNED=1 bash "$root/scripts/check-pins.sh" "$work/app.json" >"$work/output" 2>"$work/errors"; then
    echo 'FAIL  an unsigned image passed'
    exit 1
fi
grep -q '0 missing, 0 older runtime(s), 1 unsigned' "$work/output"
test ! -s "$work/errors"
echo 'ok    a published image without its signature fails'
