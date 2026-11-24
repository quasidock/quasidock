#!/usr/bin/env bash
# MARIADB_HOME=path/to/store bash tests/mariadb.sh [path/to/quasidock.exe]
# Build mariadb-sdk and mariadb first. Git Bash's OpenSSL makes test credentials.
set -euo pipefail
export MSYS_NO_PATHCONV=1
root=$(cd "$(dirname "$0")/.." && pwd)
source "$root/scripts/images.sh"
cli=${1:-${QUASIDOCK:-$root/out/bin/quasidock.exe}}
export QUASIDOCK_HOME=${MARIADB_HOME:-${BUILD_HOME:-${QUASIDOCK_HOME:-}}}
for image in mariadb-sdk mariadb; do
    if [ -z "$QUASIDOCK_HOME" ] || ! qd_cached "$cli" "$image"; then
        echo 'Build recipes/mariadb-sdk.json and recipes/mariadb.json in MARIADB_HOME first.' >&2
        exit 2
    fi
done
scratch=$(realpath "${TMPDIR:-/tmp}")
work=$(mktemp -d "$scratch/quasidock-mariadb.XXXXXX")
containers=() volumes=()
cleanup() {
    local code=$? cid log
    if [ "$code" != 0 ] && [ -n "${LOG_DIR:-}" ]; then
        mkdir -p "$LOG_DIR"
        for log in "$work"/*.log "$work/ready.err"; do
            [ ! -f "$log" ] || cp "$log" "$LOG_DIR/$(basename "$log")"
        done
    fi
    for cid in "${containers[@]}"; do
        if [ "$code" != 0 ]; then "$cli" logs "$cid" >&2 || true; fi
        if [ -n "${LOG_DIR:-}" ]; then
            mkdir -p "$LOG_DIR"
            "$cli" logs "$cid" >"$LOG_DIR/$cid.log" 2>&1 || true
        fi
        "$cli" rm -f "$cid" >/dev/null 2>&1 || true
    done
    for volume in "${volumes[@]}"; do "$cli" volume rm "$volume" >/dev/null 2>&1 || true; done
    if [ "$(dirname "$work")" = "$scratch" ] && [[ $(basename "$work") == quasidock-mariadb.* ]]; then
        rm -rf -- "$work"
    fi
    exit "$code"
}
trap cleanup EXIT
cd "$work"

ok() { printf 'ok    %s\n' "$*"; }
free_port() {
    powershell.exe -NoLogo -NoProfile -NonInteractive -Command \
        '$mariadbListener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0); try { $mariadbListener.Start(); $mariadbListener.LocalEndpoint.Port } finally { $mariadbListener.Stop() }' | tr -d '\r'
}
sql() { "$cli" exec "$cid" mariadb.exe --connect-timeout=3 --batch --skip-column-names --execute "$1" | tr -d '\r'; }
ready() {
    local n
    for n in $(seq 90); do
        if sql 'SELECT 1' >"$work/ready.out" 2>"$work/ready.err"; then return 0; fi
        [ "$("$cli" inspect -f '{{.State.Running}}' "$cid" | tr -d '\r')" = true ] || break
        sleep 0.5
    done
    cat "$work/ready.err" >&2
    echo 'MariaDB did not become ready' >&2
    return 1
}
must_fail() {
    local log=$1 status
    shift
    if timeout 30 "$@" >"$log" 2>&1; then
        echo 'An invalid MariaDB operation unexpectedly succeeded' >&2
        return 1
    else
        status=$?
    fi
    [ "$status" != 124 ] || { cat "$log" >&2; echo 'MariaDB operation timed out' >&2; return 1; }
}
ref() { printf '%s:%s@%s' "$1" "$(qd_version "$cli" "$1")" "$(qd_manifest "$cli" "$1")"; }

mkdir -p "$work/version data"
version_bind=$(cygpath -w "$work/version data")
qd_run "$cli" --rm -v "$version_bind:\${data}" mariadb --version | grep -F '12.3.4-MariaDB'
qd_run "$cli" --rm -v "$version_bind:\${data}" mariadb --help >/dev/null
[ ! -e "$work/version data/my.ini" ] && [ ! -e "$work/version data/data" ]
qd_run "$cli" --rm mariadb-sdk | grep -F '12.3.4-MariaDB'
ok 'version and help do not initialize a database'

# Build from installed headers/libraries, without the MariaDB source tree.
cp -R "$root/tests/mariadb/sdk" "$work/sdk"
recipe=$root/recipes/mariadb-sdk.json
sed -e "s|@SDK@|$(ref mariadb-sdk)|g" -e "s|@RUNTIME@|$(ref mariadb)|g" \
    -e "s|@CMAKE@|$(sed -n 's/.*"cmake": "\([^"]*\)".*/\1/p' "$recipe")|g" \
    -e "s|@LLVM@|$(sed -n 's/.*"llvm": "\([^"]*\)".*/\1/p' "$recipe")|g" \
    -e "s|@MESON@|$(sed -n 's/.*"meson": "\([^"]*\)".*/\1/p' "$recipe")|g" \
    -e "s|@BUSYBOX@|$(sed -n 's/.*"busybox": "\([^"]*\)".*/\1/p' "$recipe")|g" \
    -e 's|@SOURCE@|sdk|g' "$root/tests/mariadb/sdk.json.in" >"$work/sdk.json"
"$cli" build "$(cygpath -w "$work/sdk.json")"
# The fixture's DLL is copied through a run, using an explicit filesystem grant.
qd_run "$cli" --rm --sandbox=soft --filesystem="$(cygpath -w "$work")" \
    mariadb-sdk-probe --copy-plugin "$(cygpath -w "$work/qd_probe.dll")"

# Snapshot the runtime deployment, including directories, across every test.
store=$(cygpath -u "$QUASIDOCK_HOME")
manifest=$(qd_manifest "$cli" mariadb)
layer=$(tr -d ' \r\n' <"$store/blobs/sha256/${manifest#sha256:}" | grep -o '"layers":.*' | grep -o 'sha256:[0-9a-f]*' | tail -1 | cut -d: -f2)
deployment=$store/deploy/$layer
snapshot() {
    find "$deployment" -type d | sort
    find "$deployment" -type f -print0 | sort -z | xargs -0 sha256sum
}
snapshot >"$work/image.before"

openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=localhost \
    -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
    -keyout "$(cygpath -m "$work/server.key")" -out "$(cygpath -m "$work/server.crt")" >"$work/certificate.log" 2>&1

for sandbox in soft low appcontainer; do
    volume=mariadb-test-$sandbox-$$
    "$cli" volume create "$volume" >/dev/null
    volumes+=("$volume")
    data_win=$("$cli" volume inspect -f '{{.Mountpoint}}' "$volume" | tr -d '\r')
    data=$(cygpath -u "$data_win")
    mkdir -p "$data/plugins"
    cp "$work/qd_probe.dll" "$data/plugins/"
    cp "$deployment/lib/plugin/metadata_lock_info.dll" "$data/plugins/"
    cp "$work/server.crt" "$work/server.key" "$data/"
    port=$(free_port)
    options=(--sandbox="$sandbox" --port=mariadb="$port" -v "$volume:\${data}")
    server_args=(--plugin-dir="$data_win\plugins" --ssl-cert="$data_win\server.crt" --ssl-key="$data_win\server.key")
    cid=$(qd_run "$cli" -d "${options[@]}" mariadb "${server_args[@]}" | tr -d '\r')
    containers+=("$cid")
    ready
    sql 'SELECT VERSION()' | grep -E '^12[.]3[.]4-MariaDB'
    grep -Eq '^password=[A-Za-z0-9]{24}$' "$data/my.ini"
    credentials=$(sha256sum "$data/my.ini")
    sql "CREATE DATABASE qd_test CHARACTER SET utf8mb4; CREATE TABLE qd_test.items (id INT PRIMARY KEY, value VARCHAR(100)) ENGINE=InnoDB; INSERT INTO qd_test.items VALUES (1, 'hello 世界'); START TRANSACTION; INSERT INTO qd_test.items VALUES (2, 'rollback'); ROLLBACK"
    [ "$(sql 'SELECT COUNT(*) FROM qd_test.items')" = 1 ]
    [ "$(sql 'SELECT value FROM qd_test.items WHERE id=1')" = 'hello 世界' ]
    must_fail "$work/password.log" "$cli" exec "$cid" mariadb.exe --skip-ssl --password=incorrect --execute 'SELECT 1'
    grep -q 'Access denied' "$work/password.log"
    must_fail "$work/empty-password.log" "$cli" exec "$cid" mariadb.exe --skip-ssl --password= --execute 'SELECT 1'
    grep -q 'Access denied' "$work/empty-password.log"
    ok "$sandbox: initialization, authentication, SQL, Unicode, and transactions"

    # A separate soft container reaches the published host port, even when the
    # server is an AppContainer with a different internal listening port.
    [ "$(qd_run "$cli" --rm --sandbox=soft --port=mariadb="$port" -v "$volume:\${data}" \
        --entrypoint=mariadb.exe mariadb --batch --skip-column-names --execute 'SELECT 42' | tr -d '\r')" = 42 ]
    qd_run "$cli" --rm --sandbox="$sandbox" --port=mariadb="$port" -v "$volume:\${data}" mariadb-sdk-probe | grep -F ': 42'
    qd_run "$cli" --rm --sandbox="$sandbox" --port=mariadb="$port" -v "$volume:\${data}" \
        --entrypoint=bin/qd-client-static.exe mariadb-sdk-probe | grep -F ': 42'
    "$cli" exec "$cid" mariadb.exe --ssl --ssl-verify-server-cert --ssl-ca="$data_win\server.crt" \
        --batch --skip-column-names --execute "SHOW STATUS LIKE 'Ssl_cipher'" | grep -E 'Ssl_cipher[[:space:]]+[^[:space:]]+'
    sql "INSTALL SONAME 'qd_probe.dll'; SELECT PLUGIN_STATUS FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='qd_probe'" | grep -qx ACTIVE
    sql "SHOW STATUS LIKE 'qd_probe_started'" | grep -iE '^qd_probe_started[[:space:]]+[1-9][0-9]*$'
    sql "UNINSTALL SONAME 'qd_probe.dll'"
    [ "$(sql "SELECT COUNT(*) FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='qd_probe'")" = 0 ]
    sql "INSTALL SONAME 'metadata_lock_info.dll'; SELECT PLUGIN_STATUS FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='METADATA_LOCK_INFO'" | grep -qx ACTIVE
    sql "UNINSTALL SONAME 'metadata_lock_info.dll'"
    ok "$sandbox: published port, dynamic and static Connector/C, TLS, and bundled and SDK-built plugins"

    "$cli" exec "$cid" mariadb-dump.exe --skip-comments --databases qd_test >"$work/dump.sql"
    sql 'DROP DATABASE qd_test'
    "$cli" exec -i "$cid" mariadb.exe <"$work/dump.sql"
    [ "$(sql 'SELECT value FROM qd_test.items WHERE id=1')" = 'hello 世界' ]
    must_fail "$work/locked.log" bash "$QD_IMAGES_HELPER" run "$cli" --rm "${options[@]}" mariadb
    grep -q 'already in use' "$work/locked.log"
    "$cli" restart -t 30 "$cid"
    ready
    [ "$(sql 'SELECT value FROM qd_test.items WHERE id=1')" = 'hello 世界' ]
    [ "$(sha256sum "$data/my.ini")" = "$credentials" ]
    "$cli" stop -t 30 "$cid"
    [ "$("$cli" inspect -f '{{.State.ExitCode}}' "$cid" | tr -d '\r')" = 0 ]
    "$cli" logs "$cid" >"$work/server.log"
    secret=$(sed -n 's/^password=//p' "$data/my.ini")
    ! grep -qF "$secret" "$work/server.log"
    ok "$sandbox: dump/restore, exclusive storage, clean shutdown, and restart persistence"
done

# Reuse an initialized volume with a different published port.
port=$(free_port)
cid=$(qd_run "$cli" -d --sandbox=appcontainer --port=mariadb="$port" -v "$volume:\${data}" mariadb | tr -d '\r')
containers+=("$cid")
ready
[ "$(sql 'SELECT value FROM qd_test.items WHERE id=1')" = 'hello 世界' ]
[ "$(qd_run "$cli" --rm --sandbox=soft --port=mariadb="$port" -v "$volume:\${data}" \
    --entrypoint=mariadb.exe mariadb --batch --skip-column-names --execute 'SELECT 42' | tr -d '\r')" = 42 ]
"$cli" stop -t 30 "$cid"
[ "$(sha256sum "$data/my.ini")" = "$credentials" ]
printf '99.0\n' >"$data/mariadb.initialized"
must_fail "$work/upgrade.log" bash "$QD_IMAGES_HELPER" run "$cli" --rm -v "$volume:\${data}" mariadb
grep -q 'explicit upgrade is required' "$work/upgrade.log"
[ "$(sha256sum "$data/my.ini")" = "$credentials" ]
ok 'an existing volume can change ports, and incompatible data is not automatically upgraded'
port=$(free_port)

mkdir -p "$work/data with spaces/data"
printf 'preserve me\n' >"$work/data with spaces/data/partial"
bind=$(cygpath -w "$work/data with spaces")
must_fail "$work/partial.log" bash "$QD_IMAGES_HELPER" run "$cli" --rm -v "$bind:\${data}" mariadb
grep -q 'refusing to overwrite' "$work/partial.log"
[ "$(cat "$work/data with spaces/data/partial")" = 'preserve me' ]
rm "$work/data with spaces/data/partial"
cid=$(qd_run "$cli" -d --port=mariadb="$port" -v "$bind:\${data}" mariadb | tr -d '\r')
containers+=("$cid")
ready
"$cli" stop -t 30 "$cid"
ok 'bind paths containing spaces and preservation of incomplete initialization'

snapshot >"$work/image.after"
cmp "$work/image.before" "$work/image.after"
ok 'the runtime deployment remains unchanged'
