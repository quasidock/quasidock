#!/usr/bin/env bash
# Build recipes/nginx.json first. Needs Git Bash, Go, curl, and OpenSSL.
# NGINX_HOME=path/to/store bash tests/nginx.sh [path/to/quasidock.exe]
set -euo pipefail
export MSYS_NO_PATHCONV=1
root=$(cd "$(dirname "$0")/.." && pwd)
source "$root/scripts/images.sh"
cli=${1:-${QUASIDOCK:-$root/out/bin/quasidock.exe}}
export QUASIDOCK_HOME=${NGINX_HOME:-${BUILD_HOME:-${QUASIDOCK_HOME:-}}}
image=${NGINX_IMAGE:-nginx}
if [ -z "$QUASIDOCK_HOME" ] || ! qd_cached "$cli" "$image"; then
    echo 'Build recipes/nginx.json in NGINX_HOME (or BUILD_HOME) first.' >&2
    exit 2
fi
scratch_root=$(realpath "${TMPDIR:-/tmp}")
work=$(mktemp -d "$scratch_root/quasidock-nginx.XXXXXX")
containers=() volumes=() data_dirs=()
backend=
cleanup() {
    local code=$? cid data index log
    if [ "$code" != 0 ]; then
        for cid in "${containers[@]}"; do "$cli" logs "$cid" 2>/dev/null || true; done
        for data in "${data_dirs[@]}"; do tail -40 "$data/logs/error.log" 2>/dev/null || true; done
    fi
    if [ -n "${LOG_DIR:-}" ]; then
        mkdir -p "$LOG_DIR/nginx"
        cp "$work"/*.log "$LOG_DIR/nginx/" 2>/dev/null || true
        for index in "${!data_dirs[@]}"; do
            for log in "${data_dirs[$index]}"/logs/*.log; do
                [ ! -f "$log" ] || cp "$log" "$LOG_DIR/nginx/${volumes[$index]}-$(basename "$log")"
            done
        done
    fi
    for cid in "${containers[@]}"; do "$cli" rm -f "$cid" >/dev/null 2>&1 || true; done
    [ -z "$backend" ] || { kill "$backend" 2>/dev/null || true; wait "$backend" 2>/dev/null || true; }
    for volume in "${volumes[@]}"; do "$cli" volume rm "$volume" >/dev/null 2>&1 || true; done
    if [ "$(dirname "$work")" = "$scratch_root" ] && [[ $(basename "$work") == quasidock-nginx.* ]]; then
        rm -rf -- "$work"
    fi
    exit "$code"
}
trap cleanup EXIT
(cd "$root" && go build -o "$(cygpath -m "$work/nginx-probe.exe")" ./tests/nginx)
cd "$work"
ok() { printf 'ok    %s\n' "$*"; }
body_is() {
    local want=$1 url=$2 got
    shift 2
    got=$(curl -fsS --max-time 5 "$@" "$url")
    [ "$got" = "$want" ] || { echo "Unexpected body at $url: $got" >&2; return 1; }
}
ready() {
    local cid=$1 url=$2
    shift 2
    for _ in $(seq 60); do
        if curl -fsS --max-time 2 "$@" "$url" >/dev/null 2>&1; then return 0; fi
        [ "$("$cli" inspect -f '{{.State.Running}}' "$cid" | tr -d '\r')" = true ] || break
        sleep 0.25
    done
    echo "NGINX did not answer on $url" >&2
    return 1
}
stopped() {
    local cid=$1
    for _ in $(seq 60); do
        [ "$("$cli" inspect -f '{{.State.Running}}' "$cid" | tr -d '\r')" = true ] || return 0
        sleep 0.25
    done
    echo "NGINX did not stop: $cid" >&2
    return 1
}
must_fail() {
    local log=$1 code
    shift
    if timeout 30 bash "$QD_IMAGES_HELPER" run "$cli" "$@" >"$log" 2>&1; then
        echo 'Invalid NGINX configuration unexpectedly succeeded' >&2; return 1
    else code=$?; fi
    [ "$code" != 124 ] || { cat "$log" >&2; echo 'Invalid configuration hung' >&2; return 1; }
}

version=$(qd_run "$cli" --rm "$image" -v 2>&1)
grep -F 'nginx/1.31.10' <<<"$version"
modules=$(qd_run "$cli" --rm "$image" -V 2>&1)
for flag in --with-http_ssl_module --with-http_v2_module --with-pcre-jit; do grep -qF -- "$flag" <<<"$modules"; done
# Even an incomplete TLS environment must not interfere with informational flags.
qd_run "$cli" --rm -e NGINX_TLS_CERT_FILE=missing "$image" -v
qd_run "$cli" --rm "$image" -t
ok 'version, configuration, and required modules'

store=$(cygpath -u "$QUASIDOCK_HOME")
manifest=$(qd_manifest "$cli" "$image")
layer=$(tr -d ' \r\n' <"$store/blobs/sha256/${manifest#sha256:}" | grep -o '"layers":.*' | grep -o 'sha256:[0-9a-f]*' | tail -1 | cut -d: -f2)
deployment=$store/deploy/$layer
snapshot() {
    find "$deployment" -type d | sort
    find "$deployment" -type f -print0 | sort -z | xargs -0 sha256sum
}
snapshot >"$work/before.txt"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -subj /CN=localhost -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
    -addext 'basicConstraints=critical,CA:FALSE' -addext 'extendedKeyUsage=serverAuth' \
    -keyout "$(cygpath -m "$work/server.key")" -out "$(cygpath -m "$work/server.crt")" >certificate.log 2>&1
cert=$(cygpath -m "$work/server.crt")
base=$((51000 + RANDOM % 6000))
http_backend=$base fcgi_backend=$((base + 1))
"$work/nginx-probe.exe" serve "127.0.0.1:$http_backend" "127.0.0.1:$fcgi_backend" >backend.log 2>&1 &
backend=$!
for _ in $(seq 40); do
    if grep -q '^ready' backend.log; then break; fi
    kill -0 "$backend" || { cat backend.log >&2; exit 1; }
    sleep 0.25
done
grep -q '^ready' backend.log
base=$((base + 4))

# The default page works with no custom environment or configuration.
cid=$(qd_run "$cli" -d --port=http="$base" --port=https="$((base + 1))" "$image" | tr -d '\r')
containers+=("$cid")
ready "$cid" "http://127.0.0.1:$base/"
curl -fsS --max-time 5 "http://127.0.0.1:$base/" | grep -F 'NGINX on Quasidock'
"$cli" stop -t 5 "$cid" >/dev/null
[ "$("$cli" inspect -f '{{.State.ExitCode}}' "$cid" | tr -d '\r')" = 0 ]
ok 'default page and graceful container stop'
base=$((base + 4))

for sandbox in soft low appcontainer; do
    volume=nginx-test-$sandbox-$$
    "$cli" volume create "$volume" >/dev/null
    volumes+=("$volume")
    data_win=$("$cli" volume inspect -f '{{.Mountpoint}}' "$volume" | tr -d '\r')
    data=$(cygpath -u "$data_win")
    data_dirs+=("$data")
    document="$data/www with space Ω"
    mkdir -p "$document/protected-content" "$data/conf.d"
    printf 'nginx-%s\n' "$sandbox" >"$document/index.html"
    printf 'rewritten\n' >"$document/target.txt"
    printf '%010000d\n' 0 >"$document/compress.txt"
    printf 'private\n' >"$document/.hidden"
    printf 'private\n' >"$document/protected-content/secret.txt"
    printf 'private\n' >"$document/long-private-file.txt"
    cp "$work/server.crt" "$work/server.key" "$data/"
    cat >"$data/conf.d/test.conf" <<EOF
add_header X-Quasidock NGINX;
location ^~ /protected-content/ { deny all; }
location = /long-private-file.txt { deny all; }
rewrite ^/rewritten\$ /target.txt last;
location /proxy/ { proxy_pass http://127.0.0.1:$http_backend/; }
location /fastcgi/ {
    fastcgi_pass 127.0.0.1:$fcgi_backend;
    fastcgi_param REQUEST_METHOD \$request_method;
    fastcgi_param REQUEST_URI \$request_uri;
    fastcgi_param SERVER_PROTOCOL \$server_protocol;
}
EOF
    port=$base tls=$((base + 1)); base=$((base + 4))
    options=(--sandbox="$sandbox" --port=http="$port" --port=https="$tls" -v "$volume:\${data}"
        -e "NGINX_DOCUMENT_ROOT=$(cygpath -m "$document")"
        -e "NGINX_TLS_CERT_FILE=$data_win\server.crt" -e "NGINX_TLS_KEY_FILE=$data_win\server.key")
    qd_run "$cli" --rm "${options[@]}" "$image" -T >"$work/$sandbox-config.log" 2>&1
    grep -q 'daemon off;' "$work/$sandbox-config.log"
    cid=$(qd_run "$cli" -d "${options[@]}" "$image" | tr -d '\r')
    containers+=("$cid")
    http=http://127.0.0.1:$port https=https://127.0.0.1:$tls
    ready "$cid" "$http/"
    ready "$cid" "$https/" --cacert "$cert"
    body_is "nginx-$sandbox" "$http/"
    body_is "nginx-$sandbox" "$https/" --cacert "$cert"
    [ "$("$work/nginx-probe.exe" h2 "$https/" "$cert" | tr -d '\r')" = "nginx-$sandbox" ]
    body_is rewritten "$http/rewritten"
    body_is 'proxy POST /echo hello' "$http/proxy/echo" -d hello
    body_is 'fastcgi GET /fastcgi/test' "$http/fastcgi/test"
    headers=$(curl -fsS --max-time 5 --compressed -D - -o NUL "$http/compress.txt" | tr -d '\r')
    grep -qi '^Content-Encoding: gzip$' <<<"$headers"
    grep -qi '^X-Quasidock: NGINX$' <<<"$headers"
    denied_paths=(/.hidden /../server.key /%2e%2e/server.key /protected-content/secret.txt /long-private-file.txt /long-private-file.txt. '/target.txt::$DATA')
    # Check 8.3 aliases when NTFS has generated them; some CI volumes disable
    # short names. Both file aliases and aliases of an ancestor must be denied.
    for long in protected-content long-private-file.txt; do
        short=$("$work/nginx-probe.exe" shortpath "$(cygpath -m "$document/$long")" | tr '\\' '/' | tr -d '\r')
        alias=${short##*/}
        if [[ $alias == *'~'* ]]; then
            if [ "$long" = protected-content ]; then denied_paths+=("/$alias/secret.txt")
            else denied_paths+=("/$alias"); fi
        fi
    done
    for path in "${denied_paths[@]}"; do
        status=$(curl -sS --max-time 5 --path-as-is -o NUL -w '%{http_code}' "$http$path")
        case $status in 400|403|404) ;; *) echo "Unexpected status $status for $path" >&2; exit 1 ;; esac
    done
    ok "$sandbox: HTTP, verified TLS/HTTP2, Unicode/spaced paths, rewrite, gzip, proxy, FastCGI, and filename checks"

    # Reload must consume edited snippets, preserve generated config, and
    # leave the concurrent second container alone.
    other=$(qd_run "$cli" -d --sandbox="$sandbox" --port=http="$base" --port=https="$((base + 1))" "$image" | tr -d '\r')
    containers+=("$other")
    ready "$other" "http://127.0.0.1:$base/"
    cp "$data/conf/nginx.conf" "$work/$sandbox-before.conf"
    printf 'location = /reloaded { return 200 "reloaded"; }\n' >"$data/conf.d/reload.conf"
    "$cli" exec "$cid" nginx.exe -s reload
    for _ in $(seq 40); do
        if curl -fsS --max-time 2 "$http/reloaded" 2>/dev/null | grep -qx reloaded; then break; fi
        sleep 0.25
    done
    body_is reloaded "$http/reloaded"
    cmp "$data/conf/nginx.conf" "$work/$sandbox-before.conf"
    "$cli" exec "$cid" nginx.exe -s reopen
    "$cli" exec "$cid" nginx.exe -s quit
    stopped "$cid"
    [ "$("$cli" inspect -f '{{.State.ExitCode}}' "$cid" | tr -d '\r')" = 0 ]
    curl -fsS --max-time 5 "http://127.0.0.1:$base/" | grep -q 'NGINX on Quasidock'
    "$cli" exec "$other" nginx.exe -s stop
    stopped "$other"
    base=$((base + 4))
    "$cli" start "$cid" >/dev/null
    ready "$cid" "$https/" --cacert "$cert"
    "$cli" restart -t 5 "$cid" >/dev/null
    ready "$cid" "$https/" --cacert "$cert"
    body_is reloaded "$http/reloaded"
    "$cli" stop -t 5 "$cid" >/dev/null
    [ "$("$cli" inspect -f '{{.State.ExitCode}}' "$cid" | tr -d '\r')" = 0 ]
    if curl -fsS --max-time 2 "$http/" >/dev/null 2>&1; then echo 'HTTP listener survived stop' >&2; exit 1; fi
    if curl -fsS --max-time 2 --cacert "$cert" "$https/" >/dev/null 2>&1; then echo 'TLS listener survived stop' >&2; exit 1; fi
    [ -s "$data/logs/error.log" ] && [ -s "$data/logs/access.log" ]
    ok "$sandbox: reload/reopen/quit/stop, concurrent isolation, restart, and data logs"

    # Custom files are passed through unchanged, with relative -c resolved
    # against the explicitly supplied prefix. It reuses the validated config.
    cp "$data/conf/nginx.conf" "$data/custom.conf"
    before=$(sha256sum "$data/custom.conf")
    qd_run "$cli" --rm "${options[@]}" "$image" -p "$data_win/" -c custom.conf -t
    [ "$(sha256sum "$data/custom.conf")" = "$before" ]
    must_fail "$work/$sandbox-incomplete.log" --rm --sandbox="$sandbox" -e NGINX_TLS_CERT_FILE=missing "$image" -t
    must_fail "$work/$sandbox-missing.log" --rm "${options[@]}" -e NGINX_TLS_CERT_FILE=missing "$image" -t
    must_fail "$work/$sandbox-invalid.log" --rm "${options[@]}" -e "NGINX_TLS_CERT_FILE=$(cygpath -m "$document/index.html")" "$image" -t
    printf 'invalid_directive;\n' >"$data/bad.conf"
    must_fail "$work/$sandbox-bad-config.log" --rm "${options[@]}" "$image" -c "$data_win\bad.conf" -t
    ok "$sandbox: custom config and prompt configuration/TLS failures"
done

mkdir "$work/private"
printf 'outside-grants\n' >"$work/private/index.html"
cid=$(qd_run "$cli" -d --sandbox=appcontainer --nofilesystem=cwd \
    --port=http="$base" --port=https="$((base + 1))" \
    -e "NGINX_DOCUMENT_ROOT=$(cygpath -m "$work/private")" "$image" | tr -d '\r')
containers+=("$cid")
for _ in $(seq 60); do
    status=$(curl -sS --max-time 2 -o "$(cygpath -m "$work/denied.txt")" -w '%{http_code}' "http://127.0.0.1:$base/" 2>/dev/null) || true
    [ "$status" = 000 ] || break
    sleep 0.25
done
[ "$status" = 403 ]
[ -f "$work/denied.txt" ]
! grep -q outside-grants "$work/denied.txt"
"$cli" stop -t 5 "$cid" >/dev/null
ok 'AppContainer rejects content outside its filesystem grants'
snapshot >"$work/after.txt"
cmp "$work/before.txt" "$work/after.txt"
ok 'server runs leave the immutable deployment unchanged'
