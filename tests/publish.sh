#!/usr/bin/env bash
# Exercise the CI staging contract without a registry, Go, credentials, or builds.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -f "$work/cli" "$work/calls" "$work/output"; rmdir "$work"' EXIT
export PUBLISH_CALLS="$work/calls"
export QUASIDOCK_REGISTRY=ghcr.io/quasidock/quasidock
export QUASIDOCK="$work/cli"
cat >"$QUASIDOCK" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$PUBLISH_CALLS"
if [ "${PUBLISH_FAIL_TAG:-0}" = 1 ] && [ "$1" = tag ]; then exit 1; fi
EOF
chmod +x "$QUASIDOCK"
bash "$root/scripts/publish.sh" --stage ci-123 busybox
test "$(cat "$PUBLISH_CALLS")" = $'tag busybox ghcr.io/quasidock/quasidock/busybox:ci-123\npush --sources ghcr.io/quasidock/quasidock/busybox:ci-123'
echo 'ok    staging pushes sources to the candidate tag only'
for rejected in latest 1.0 ci-not-a-run; do
    : >"$PUBLISH_CALLS"
    if bash "$root/scripts/publish.sh" --stage "$rejected" busybox >"$work/output" 2>&1; then exit 1; fi
    test ! -s "$PUBLISH_CALLS"
done
echo 'ok    invalid or release tags are rejected before CLI calls'
: >"$PUBLISH_CALLS"
if bash "$root/scripts/publish.sh" --stage ci-123 not-in-the-allowlist >"$work/output" 2>&1; then exit 1; fi
test ! -s "$PUBLISH_CALLS"
echo 'ok    images outside the publish allowlist cannot be staged'
: >"$PUBLISH_CALLS"
if PUBLISH_FAIL_TAG=1 bash "$root/scripts/publish.sh" --stage ci-123 busybox >"$work/output" 2>&1; then exit 1; fi
test "$(cat "$PUBLISH_CALLS")" = 'tag busybox ghcr.io/quasidock/quasidock/busybox:ci-123'
echo 'ok    a failed tag stops before upload'
echo '0 check(s) failed.'
