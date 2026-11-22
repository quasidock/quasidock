#!/usr/bin/env bash
# Native Apache HTTP/TLS integration. Build recipes/apache.json first.
# APACHE_HOME=path/to/store bash tests/apache.sh [path/to/quasidock.exe]
# Needs Git Bash, curl, and its OpenSSL command (only for test certificates).
set -euo pipefail
export MSYS_NO_PATHCONV=1
root=$(cd "$(dirname "$0")/.." && pwd)
source "$root/scripts/images.sh"
cli=${1:-${QUASIDOCK:-$root/out/bin/quasidock.exe}}
export QUASIDOCK_HOME=${APACHE_HOME:-${BUILD_HOME:-${QUASIDOCK_HOME:-}}}
image=${APACHE_IMAGE:-apache}
if [ -z "$QUASIDOCK_HOME" ] || ! qd_cached "$cli" "$image"; then
    echo 'Build recipes/apache.json in APACHE_HOME (or BUILD_HOME) first.' >&2
    exit 2
fi
scratch_root=$(realpath "${TMPDIR:-/tmp}")
work=$(mktemp -d "$scratch_root/quasidock-apache.XXXXXX")
containers=() volumes=() data_dirs=()
cleanup() {
    local code=$? cid data index log
    if [ "$code" != 0 ]; then
        for cid in "${containers[@]}"; do "$cli" logs "$cid" 2>/dev/null || true; done
        for data in "${data_dirs[@]}"; do cat "$data/error.log" 2>/dev/null || true; done
    fi
    if [ -n "${LOG_DIR:-}" ]; then
        mkdir -p "$LOG_DIR/apache"
        for index in "${!data_dirs[@]}"; do
            for log in "${data_dirs[$index]}"/*.log; do
                [ ! -f "$log" ] || cp "$log" "$LOG_DIR/apache/${volumes[$index]}-$(basename "$log")"
            done
        done
    fi
    for cid in "${containers[@]}"; do "$cli" rm -f "$cid" >/dev/null 2>&1 || true; done
    for volume in "${volumes[@]}"; do "$cli" volume rm "$volume" >/dev/null 2>&1 || true; done
    if [ "$(dirname "$work")" = "$scratch_root" ] && [[ $(basename "$work") == quasidock-apache.* ]]; then
        rm -rf -- "$work"
    fi
    exit "$code"
}
trap cleanup EXIT
cd "$work"

ok() { printf 'ok    %s\n' "$*"; }
body_is() {
    local want=$1 url=$2 got
    shift 2
    got=$(curl -fsS --max-time 5 "$@" "$url")
    [ "$got" = "$want" ] || { echo "Unexpected body at $url: $got" >&2; return 1; }
}
ready() {
    local cid=$1 url=$2 i
    shift 2
    for i in $(seq 60); do
        if curl -fsS --max-time 2 "$@" "$url" >/dev/null 2>&1; then return 0; fi
        [ "$("$cli" inspect -f '{{.State.Running}}' "$cid" | tr -d '\r')" = true ] || break
        sleep 0.25
    done
    echo "Apache did not answer on $url" >&2
    return 1
}
must_fail() {
    local log=$1 code
    shift
    if timeout 30 bash "$QD_IMAGES_HELPER" run "$cli" "$@" >"$log" 2>&1; then
        echo 'An invalid Apache configuration unexpectedly succeeded' >&2
        return 1
    else
        code=$?
    fi
    if [ "$code" = 124 ]; then cat "$log" >&2; echo 'Invalid configuration hung instead of failing' >&2; return 1; fi
}

qd_run "$cli" --rm "$image" -v | grep -F 'Apache/2.4.71'
qd_run "$cli" --rm "$image" -t
modules=$(qd_run "$cli" --rm "$image" -M)
for module in rewrite expires headers deflate proxy_http proxy_fcgi proxy_wstunnel ssl socache_shmcb; do
    grep -qw "${module}_module" <<<"$modules"
done
ok 'version, configuration, and required modules'

# The running server must leave the immutable deployment unchanged.
store=$(cygpath -u "$QUASIDOCK_HOME")
manifest=$(qd_manifest "$cli" "$image")
layer=$(tr -d ' \r\n' <"$store/blobs/sha256/${manifest#sha256:}" | grep -o '"layers":.*' | grep -o 'sha256:[0-9a-f]*' | tail -1 | cut -d: -f2)
deployment=$store/deploy/$layer
snapshot() {
    find "$deployment" -type d | sort
    find "$deployment" -type f -print0 | sort -z | xargs -0 sha256sum
}
snapshot >"$work/deployment-before.txt"

# Generate credentials per test, never in the image or the checkout.
cert=$(cygpath -m "$work/server.crt")
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -subj /CN=localhost -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
    -addext 'basicConstraints=critical,CA:FALSE' -addext 'extendedKeyUsage=serverAuth' \
    -keyout "$(cygpath -m "$work/server.key")" -out "$cert" >"$work/certificate.log" 2>&1 || { cat "$work/certificate.log" >&2; exit 1; }

base=$((51000 + RANDOM % 7000))
for sandbox in soft low appcontainer; do
    volume=apache-test-$sandbox-$$
    "$cli" volume create "$volume" >/dev/null
    volumes+=("$volume")
    data_win=$("$cli" volume inspect -f '{{.Mountpoint}}' "$volume" | tr -d '\r')
    data=$(cygpath -u "$data_win")
    data_dirs+=("$data")
    mkdir -p "$data/www with space" "$data/conf.d"
    printf 'apache-%s\n' "$sandbox" >"$data/www with space/index.html"
    printf 'rewritten\n' >"$data/www with space/target.txt"
    printf '%010000d\n' 0 >"$data/www with space/compress.txt"
    printf 'Require all denied\n' >"$data/www with space/.htaccess"
    mkdir -p "$data/www with space/no-index"
    cp "$work/server.crt" "$work/server.key" "$data/"
    cat >"$data/conf.d/test.conf" <<'EOF'
Header set X-Quasidock Apache
ExpiresActive On
ExpiresDefault "access plus 60 seconds"
AddOutputFilterByType DEFLATE text/plain text/html
RewriteEngine On
RewriteRule ^/rewritten$ /target.txt [PT,L]
EOF
    port=$base tls=$((base + 1))
    base=$((base + 4))
    options=(--sandbox="$sandbox" --port=http="$port" --port=https="$tls" -v "$volume:\${data}"
        -e "HTTPD_DOCUMENT_ROOT=$data_win\www with space"
        -e "HTTPD_TLS_CERT_FILE=$data_win\server.crt" -e "HTTPD_TLS_KEY_FILE=$data_win\server.key")
    arguments=(-D TLS)
    if [ "$sandbox" = low ]; then
        printf 'Include conf/httpd.conf\nHeader set X-Custom-Config yes\n' >"$data/custom-httpd.conf"
        arguments+=(-f "$data_win\custom-httpd.conf")
    fi
    cid=$(qd_run "$cli" -d "${options[@]}" "$image" "${arguments[@]}" | tr -d '\r')
    containers+=("$cid")
    http=http://127.0.0.1:$port
    https=https://127.0.0.1:$tls
    ready "$cid" "$http/"
    ready "$cid" "$https/" --cacert "$cert"
    body_is "apache-$sandbox" "$http/"
    body_is "apache-$sandbox" "$https/" --cacert "$cert"
    body_is rewritten "$http/rewritten"
    headers=$(curl -fsS --max-time 5 -D - -o NUL "$http/target.txt" | tr -d '\r')
    grep -qi '^X-Quasidock: Apache$' <<<"$headers"
    grep -qi '^Expires:' <<<"$headers"
    if [ "$sandbox" = low ]; then grep -qi '^X-Custom-Config: yes$' <<<"$headers"; fi
    headers=$(curl -fsS --max-time 5 --compressed -D - -o NUL "$http/compress.txt" | tr -d '\r')
    grep -qi '^Content-Encoding: gzip$' <<<"$headers"
    for path in /.htaccess /no-index/ /../server.key /%2e%2e/server.key; do
        status=$(curl -sS --max-time 5 --path-as-is -o NUL -w '%{http_code}' "$http$path")
        case $status in 400|403|404) ;; *) echo "Unexpected status $status for $path" >&2; exit 1 ;; esac
    done
    ok "$sandbox: HTTP, verified TLS, custom document root, rewrite, headers, expires, and gzip"

    "$cli" restart -t 1 "$cid" >/dev/null
    ready "$cid" "$https/" --cacert "$cert"
    body_is "apache-$sandbox" "$http/"
    "$cli" stop -t 1 "$cid" >/dev/null
    [ "$("$cli" inspect -f '{{.State.Running}}' "$cid" | tr -d '\r')" = false ]
    if curl -fsS --max-time 2 "$http/" >/dev/null 2>&1; then echo 'HTTP listener survived stop' >&2; exit 1; fi
    if curl -fsS --max-time 2 --cacert "$cert" "$https/" >/dev/null 2>&1; then echo 'TLS listener survived stop' >&2; exit 1; fi
    [ -s "$data/error.log" ] && [ -s "$data/access.log" ]
    ok "$sandbox: restart preserves configuration; stop ends listeners; logs are in data"
done

# Two servers at once also exercise the reverse proxy without a separate
# host tool or a loopback dependency between different AppContainers.
backend=$(qd_run "$cli" -d --sandbox=soft --port=http="$base" --port=https="$((base + 1))" "$image" | tr -d '\r')
containers+=("$backend")
ready "$backend" "http://127.0.0.1:$base/"
printf 'ProxyPass /proxy/ http://127.0.0.1:%s/\n' "$base" >"$data/conf.d/proxy.conf"
frontend=$(qd_run "$cli" -d --sandbox=soft --port=http="$((base + 2))" --port=https="$((base + 3))" \
    -v "$volume:\${data}" "$image" | tr -d '\r')
containers+=("$frontend")
ready "$frontend" "http://127.0.0.1:$((base + 2))/proxy/"
curl -fsS --max-time 5 "http://127.0.0.1:$((base + 2))/proxy/" | grep -F 'Apache on Quasidock'
ok 'concurrent containers and HTTP reverse proxy'

must_fail "$work/missing.log" --rm --port=http="$((base + 4))" --port=https="$((base + 5))" --entrypoint=apache "$image" -t -D TLS
must_fail "$work/invalid.log" --rm --port=http="$((base + 4))" --port=https="$((base + 5))" \
    -v "$volume:\${data}" -e "HTTPD_TLS_CERT_FILE=$data_win\www with space\index.html" \
    -e "HTTPD_TLS_KEY_FILE=$data_win\server.key" --entrypoint=apache "$image" -D TLS
ok 'missing and invalid TLS credentials fail'

mkdir "$work/private"
printf 'private\n' >"$work/private/index.html"
must_fail "$work/denied.log" --rm --sandbox=appcontainer --nofilesystem=cwd \
    --port=http="$((base + 4))" --port=https="$((base + 5))" \
    -e "HTTPD_DOCUMENT_ROOT=$(cygpath -m "$work/private")" --entrypoint=apache "$image" -t
grep -q 'DocumentRoot' "$work/denied.log"
ok 'AppContainer still rejects a document root outside its filesystem grants'

snapshot >"$work/deployment-after.txt"
cmp "$work/deployment-before.txt" "$work/deployment-after.txt"
ok 'server runs leave the immutable deployment unchanged'
