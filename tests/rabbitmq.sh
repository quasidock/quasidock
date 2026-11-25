#!/usr/bin/env bash
# RABBITMQ_HOME=path/to/store bash tests/rabbitmq.sh [path/to/quasidock.exe]
set -euo pipefail
export MSYS_NO_PATHCONV=1
root=$(cd "$(dirname "$0")/.." && pwd)
source "$root/scripts/images.sh"
cli=${1:-${QUASIDOCK:-$root/out/bin/quasidock.exe}}
export QUASIDOCK_HOME=${RABBITMQ_HOME:-${QUASIDOCK_HOME:-}}
image=${RABBITMQ_IMAGE:-rabbitmq}
qd_cached "$cli" "$image" || { echo 'Build erlang and rabbitmq in RABBITMQ_HOME first.' >&2; exit 2; }
scratch=$(realpath "${TMPDIR:-/tmp}")
work=$(mktemp -d "$scratch/quasidock-rabbitmq.XXXXXX")
containers=() volumes=()
cleanup() {
    local code=$? cid
    for cid in "${containers[@]}"; do
        if [ "$code" != 0 ]; then "$cli" logs "$cid" >&2 || true; fi
        if [ -n "${LOG_DIR:-}" ]; then mkdir -p "$LOG_DIR"; "$cli" logs "$cid" >"$LOG_DIR/$cid.log" 2>&1 || true; fi
        "$cli" rm -f "$cid" >/dev/null 2>&1 || true
    done
    for volume in "${volumes[@]}"; do "$cli" volume rm "$volume" >/dev/null 2>&1 || true; done
    case "$work" in "$scratch"/quasidock-rabbitmq.*) rm -rf -- "$work";; esac
    exit "$code"
}
trap cleanup EXIT
cd "$work"
free_port() {
    powershell.exe -NoLogo -NoProfile -NonInteractive -Command '$l=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0); try {$l.Start(); $l.LocalEndpoint.Port} finally {$l.Stop()}' | tr -d '\r'
}
auth_of() {
    QD_RABBITMQ_CREDENTIALS="$(cygpath -w "$1/credentials.json")" powershell.exe -NoLogo -NoProfile -NonInteractive -Command '$c=Get-Content -LiteralPath $env:QD_RABBITMQ_CREDENTIALS -Raw | ConvertFrom-Json; Write-Output ($c.username + ":" + $c.password)' | tr -d '\r'
}
ready() {
    local cid=$1 data=$2 port=$3 i auth
    for i in $(seq 120); do
        if [ -s "$data/credentials.json" ]; then
            auth=$(auth_of "$data")
            if curl -fsS --max-time 2 -u "$auth" "http://127.0.0.1:$port/api/overview" >"$work/overview.json" 2>/dev/null; then return 0; fi
        fi
        [ "$("$cli" inspect -f '{{.State.Running}}' "$cid" | tr -d '\r')" = true ] || break
        sleep 0.5
    done
    "$cli" logs "$cid" >&2
    echo 'RabbitMQ did not become ready' >&2
    return 1
}
must_fail() {
    local code
    if timeout 30 "$@" >"$work/expected-error.log" 2>&1; then cat "$work/expected-error.log" >&2; return 1; else code=$?; fi
    [ "$code" != 124 ]
}
amqp() {
    local data
    data=$(qd_container_data "$cli" "$1")
    "$cli" exec "$1" escript.exe "$data/amqp.escript" "$2" | grep -q "${2}_ok"
}
mkdir -p 'version data'
version_bind=$(cygpath -w "$work/version data")
"$cli" run --rm -v "$version_bind:\${data}" --entrypoint=rabbitmq-server.exe "$image" --version | grep -q '4.4.1'
"$cli" run --rm -v "$version_bind:\${data}" --entrypoint=rabbitmq-server.exe "$image" --help >/dev/null
[ ! -e 'version data/credentials.json' ]
echo 'ok    version and help do not initialize broker data'
for sandbox in ${RABBITMQ_SANDBOXES:-low soft appcontainer}; do
    volume=quasidock-rabbitmq-$$-$sandbox
    "$cli" volume create "$volume" >/dev/null
    volumes+=("$volume")
    data=$("$cli" volume inspect -f '{{.Mountpoint}}' "$volume" | tr -d '\r')
    data=$(cygpath -u "$data")
    cp "$root/tests/rabbitmq/amqp.escript" "$data/amqp.escript"
    mkdir -p "$work/$sandbox bind"
    bind=$(cygpath -w "$work/$sandbox bind")
    port=$(free_port); management=$(free_port)
    port2=$(free_port); management2=$(free_port)
    cid=$("$cli" run -d --sandbox="$sandbox" --port=amqp="$port" --port=management="$management" -v "$volume:\${data}" "$image" | tr -d '\r')
    containers+=("$cid")
    ready "$cid" "$data" "$management"
    auth=$(auth_of "$data")
    curl -fsS -u "$auth" "http://127.0.0.1:$management/" | grep -q 'RabbitMQ'
    must_fail curl -fsS -u admin:incorrect "http://127.0.0.1:$management/api/overview"
    "$cli" exec "$cid" rabbitmq-diagnostics.exe -q ping | grep -q 'Ping succeeded'
    "$cli" exec "$cid" rabbitmq-plugins.exe list -e | grep -q rabbitmq_management
    amqp "$cid" unauthorized
    "$cli" run --rm --sandbox=soft --port=amqp="$port" -v "$volume:\${data}" --entrypoint=escript.exe "$image" "$(cygpath -w "$data/amqp.escript")" publish | grep -q publish_ok
    second=$("$cli" run -d --sandbox="$sandbox" --port=amqp="$port2" --port=management="$management2" -v "$bind:\${data}" "$image" | tr -d '\r')
    containers+=("$second")
    ready "$second" "$work/$sandbox bind" "$management2"
    ! cmp -s "$data/node.txt" "$work/$sandbox bind/node.txt"
    ! cmp -s "$data/credentials.json" "$work/$sandbox bind/credentials.json"
    must_fail "$cli" run --rm --sandbox="$sandbox" --port=amqp="$(free_port)" --port=management="$(free_port)" -v "$volume:\${data}" "$image"
    "$cli" restart -t 30 "$cid" >/dev/null
    ready "$cid" "$data" "$management"
    amqp "$cid" consume
    amqp "$cid" publish
    "$cli" stop -t 30 "$cid" >/dev/null
    [ "$("$cli" inspect -f '{{.State.ExitCode}}' "$cid" | tr -d '\r')" = 0 ]
    ready "$second" "$work/$sandbox bind" "$management2"
    "$cli" exec "$second" rabbitmq-diagnostics.exe -q ping | grep -q 'Ping succeeded'
    "$cli" rm "$cid" >/dev/null
    replacement=$("$cli" run -d --sandbox="$sandbox" --port=amqp="$port" --port=management="$management" -v "$volume:\${data}" "$image" | tr -d '\r')
    containers+=("$replacement")
    ready "$replacement" "$data" "$management"
    [ "$(auth_of "$data")" = "$auth" ]
    amqp "$replacement" consume
    "$cli" stop -t 30 "$replacement" "$second" >/dev/null
    echo "ok    $sandbox: AMQP durability, management, CLI, independent brokers, shutdown and volume reuse"
done
