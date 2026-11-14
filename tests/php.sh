#!/usr/bin/env bash
# PHP end to end: PHP's runtime image (recipes/php.json) runs PHP's official
# Windows build with its php.ini and extensions under each sandbox. The
# extension images (recipes/php-apcu.json, php-xdebug.json, php-igbinary.json,
# php-redis.json, php-ssh2.json, php-yaml.json, php-zstd.json), which the SDK
# image's php-ext builds with zig, yaml's and zstd's with libraries their
# recipes build first, join PHP's runs once installed, through PHP's
# extension point, under each sandbox, and pass their own test suites:
# ssh2's and redis's against servers of tests/php/servers, built with Go, as
# this machine has none. An app that lists PHP, APCu and Xdebug as runtimes
# loads those through their .ini files, the installed others too, and
# Xdebug's step debugger connects to a DBGp client from it, in every sandbox,
# and in an AppContainer without the loopback exemption, through zigsaw.
# Composer's image
# (recipes/composer.json) installs a project offline; Kanboard
# (recipes/kanboard.json), whose dependencies a build installed with it,
# serves its pages and API with PHP's web server, under soft only.
#
# Not in CI: tests/build.sh's PHP section is, with a small test extension.
#
#   PHP_HOME=path\to\store bash tests/php.sh [path\to\quasidock.exe]
#
# It builds the PHP images into the store unless it has them, and removes
# what it installed afterwards. The extensions' tests come from their
# release tarballs, which their builds left in the store's downloads;
# Xdebug's from its GitHub tag, as its PECL tarball has none (SEED_DOWNLOADS
# may have it). About three minutes once the images are built.

set -u
export MSYS_NO_PATHCONV=1
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/images.sh"

root=$(cygpath -w "$(cd "$(dirname "$0")/.." && pwd)")
zigsaw=${1:-$root/out/bin/quasidock.exe}
QUASIDOCK_HOME=${PHP_HOME:-${BUILD_HOME:-}}
if [ -z "$QUASIDOCK_HOME" ]; then
    echo "tests/php.sh needs a store in PHP_HOME or BUILD_HOME; it builds PHP's images there unless it has them."
    exit 1
fi
export QUASIDOCK_HOME
case "$QUASIDOCK_HOME" in *' '*) echo "PHP_HOME can't have a space: run-tests.php splits its options at spaces"; exit 1 ;; esac
work=$(cygpath -w "$(mktemp -d)")
XDEBUG_TESTS_URL=https://github.com/xdebug/xdebug/archive/refs/tags/3.5.3.tar.gz
XDEBUG_TESTS_SHA256=954e56021668121ecc50b92d2ad1ce945f22ecf81ffc5bb5835219485b12ef5f

failures=0
check() {
    local label=$1
    shift
    local out
    if out=$("$@" 2>&1); then
        printf 'ok    %s\n' "$label"
    else
        printf 'FAIL  %s\n      %s\n' "$label" "$(grep -v '^\s*$' <<<"$out" | tail -1 | cut -c1-110)"
        failures=$((failures + 1))
    fi
}

z() { "$zigsaw" "$@"; }
manifest_of() { qd_manifest "$zigsaw" "$1"; }
own_layer() { grep -o 'sha256:[0-9a-f]*' "$QUASIDOCK_HOME\\blobs\\sha256\\$(manifest_of "$1" | cut -d: -f2)" | tail -1 | cut -d: -f2; }
deploy_of() { echo "$QUASIDOCK_HOME\\deploy\\$(own_layer "$1")"; }
recipe_sha256() { grep -o '"sha256": *"[0-9a-f]*"' "$root\\recipes\\$1" | head -1 | cut -d'"' -f4; }

PHP=php SDK=php-sdk COMPOSER=composer KANBOARD=kanboard
installed=()
for id in $SDK $PHP $COMPOSER $KANBOARD; do
    qd_cached "$zigsaw" "$id" || installed+=("$id")
done

check "php-sdk.json builds PHP's SDK image" z build "$root\\recipes\\php-sdk.json"
check "php.json builds PHP's runtime image from it" z build "$root\\recipes\\php.json"
check "composer.json builds Composer's image, on PHP's runtime" z build "$root\\recipes\\composer.json"
check "kanboard.json builds Kanboard, its dependencies installed by Composer's alias" z build "$root\\recipes\\kanboard.json"

# --- the runtime ---------------------------------------------------------------

mkdir -p "$work\\cwd"
php_dir=$(deploy_of $PHP)
runtime_runs() {
    local sb out
    for sb in soft low appcontainer; do
        out=$(cd "$work\\cwd" && qd_run z --sandbox=$sb $PHP -r 'echo PHP_VERSION, " ", php_ini_loaded_file(), "\n"; foreach (["curl", "fileinfo", "gd", "intl", "mbstring", "openssl", "pdo_sqlite", "sqlite3", "sodium", "zip"] as $e) if (!extension_loaded($e)) echo "no $e\n";' | tr -d '\r')
        [ "$out" = "8.5.12 $php_dir\\php.ini" ] || { echo "$sb, got: $out"; return 1; }
    done
}
check "the runtime runs PHP with its php.ini and extensions, under soft, low and appcontainer" runtime_runs
runtime_files() {
    [ -f "$php_dir\\php.exe" ] && [ -f "$php_dir\\ext\\php_curl.dll" ] || { echo "missing PHP's files"; return 1; }
    [ ! -e "$php_dir\\dev" ] || { echo "has the SDK's dev"; return 1; }
}
check "it has PHP's files, without the SDK's headers and php-ext" runtime_files
https_works() {
    local out
    out=$(cd "$work\\cwd" && qd_run z $PHP -r '$c = curl_init("https://www.php.net/"); curl_setopt($c, CURLOPT_RETURNTRANSFER, true); echo curl_exec($c) === false ? curl_error($c) : "curl ok", "\n"; echo strlen((string) file_get_contents("https://www.php.net/")) > 1000 ? "stream ok" : "stream fails", "\n";' 2>&1 | tr -d '\r')
    [ "$out" = "curl ok
stream ok" ] || { echo "got: $out"; return 1; }
}
check "HTTPS works through curl (Windows' roots, in the file \${roots} names) and through streams (Windows' stores)" https_works
# curl's roots are that file's: with another one, whose root isn't
# php.net's (a self-signed certificate of Git Bash's openssl), it refuses.
curl_roots() {
    local out
    openssl req -x509 -newkey rsa:2048 -nodes -keyout "$work\\other.key" -out "$work\\cwd\\other.pem" -days 1 -subj "/CN=zigsaw php.sh" >/dev/null 2>&1 ||
        { echo "openssl (Git Bash's) made no certificate"; return 1; }
    out=$(cd "$work\\cwd" && qd_run z "--env=SSL_CERT_FILE=$work\\cwd\\other.pem" $PHP -r '$c = curl_init("https://www.php.net/"); curl_setopt($c, CURLOPT_RETURNTRANSFER, true); echo curl_exec($c) === false ? curl_error($c) : "curl ok", "\n";' 2>&1 | tr -d '\r')
    grep -q 'SSL certificate' <<<"$out" || { echo "got: $out"; return 1; }
}
check "curl's roots are the ones SSL_CERT_FILE names" curl_roots

# --- the extension images ------------------------------------------------------

# Its DLL, its .ini, about.bat, the DLL's import library, and the headers
# its config.w32 installs, for extensions that build with it (APCu's and
# igbinary's).
# yaml's and zstd's have the licence of the library linked into their DLL,
# and not its headers or static library.


# APCu's tests, from its release tarball, which its build left in the
# store's downloads.
# php-ext test's output ends with how many failed; run-tests.php's summary
# says how many passed, which mustn't be none.

# Xdebug's tests, from its GitHub tag, with its own copy of run-tests.php,
# which knows the {TMPFILE:} placeholders and the AFTER section they use.
# Under soft: its tests write logs where Xdebug's own CI may, such as
# C:\Windows\Temp, which a low-integrity run can't.
# remote_log-win writes its log into Windows' Temp and deletes it, which it
# can't while Xdebug has it open, so it passes only where the file isn't
# there yet: it goes before and after.

# igbinary's, ssh2's and redis's tests, from their release tarballs.

# yaml's and zstd's, from their PECL tarballs (their recipes' second source:
# the first is their library's).

# The SSH server takes a password, or the key ssh2's tests have, and runs
# commands with BusyBox's sh; Redis is miniredis. Each says its port.
# ssh2_send_signal.phpt traps SIGINT in bash, which Windows has neither of;
# with MSVC's build, it fails the same way.
# redis's own runner stops at the first Error, and miniredis lacks some of
# what tests read: tests/php/redis-each.php runs each test on its own. MSVC
# 14.44's build gets the same results, test by test (docs/findings.md).

# --- an app with the extensions as runtimes ------------------------------------

# Xdebug connects to the listener, a DBGp client run by PHP's runtime, which
# takes the connection and tells it to run.
    # Xdebug's image uses port 9003, dbgp, which --port moves.
# The consumer has the network: its container is exempt from loopback
# isolation while it runs. Without the exemption, zigsaw forwards the port.

# --- Composer and Kanboard ----------------------------------------------------

# A project that requires a package from a path repository, without
# Packagist: Composer installs it offline. Its script runs composer by name.
composer_installs() {
    local sb=$1 out
    rm -rf "$work\\composer" && cp -r "$(cygpath -u "$root")/tests/php/composer" "$(cygpath -u "$work")/composer"
    out=$(cd "$work\\composer" && qd_run z --sandbox=$sb --rm $COMPOSER install --no-interaction --no-ansi 2>&1 | tr -d '\r')
    grep -q 'Installing zigsaw/hello (1.0.0)' <<<"$out" || { echo "$sb: $(tail -1 <<<"$out")"; return 1; }
    out=$(cd "$work\\composer" && qd_run z --sandbox=$sb --rm $COMPOSER run-script which --no-ansi 2>&1 | tr -d '\r')
    grep -q '^Composer version 2\.10\.3' <<<"$out" || { echo "$sb, its script: $(tail -1 <<<"$out")"; return 1; }
    # Without what zigsaw says about grants: the run's grant of the working
    # directory under low goes when it ends.
    out=$(cd "$work\\composer" && qd_run z --sandbox=$sb $PHP -r 'require "vendor/autoload.php"; echo Zigsaw\Hello\Hello::hi(), "\n";' 2>&1 | tr -d '\r' |
        grep -v -e '^labelled ' -e '^removed the low integrity label ')
    [ "$out" = "hello from a package Composer installed" ] || { echo "$sb, PHP: $out"; return 1; }
}
check "Composer installs a project offline, and runs its script, under soft" composer_installs soft
check "and under low" composer_installs low

# Kanboard's web server, its service on a port of its own, without opening a
# browser.
kanboard_port=$((9100 + RANDOM % 800))
kanboard_start() {
    bash "$QD_IMAGES_HELPER" run "$zigsaw" --sandbox=$1 --rm --port=http=$kanboard_port $KANBOARD --no-browser >"$work\\kanboard.out" 2>&1 &
    kanboard=$!
    for _ in $(seq 40); do grep -q 'Development Server' "$work\\kanboard.out" 2>/dev/null && break; sleep 0.25; done
}
kanboard_stop() { kill $kanboard 2>/dev/null; wait $kanboard 2>/dev/null; }
kb() { curl -s -m 10 -c "$work\\jar" -b "$work\\jar" "$@"; }
kanboard_serves() {
    local url="http://127.0.0.1:$kanboard_port" csrf out p
    csrf=$(kb "$url/?controller=AuthController&action=login" | sed -n 's/.*name="csrf_token" value="\([^"]*\)".*/\1/p' | head -1)
    [ -n "$csrf" ] || { echo "no login form"; return 1; }
    out=$(kb -o /dev/null -w '%{http_code} %{redirect_url}' --data-urlencode "csrf_token=$csrf" -d username=admin -d password=admin "$url/?controller=AuthController&action=check")
    [ "$out" = "302 $url/?controller=DashboardController&action=show" ] || { echo "logging in: $out"; return 1; }
    out=$(kb -u admin:admin -d '{"jsonrpc":"2.0","method":"createProject","id":1,"params":{"name":"zigsaw"}}' "$url/jsonrpc.php")
    [ "$out" = '{"jsonrpc":"2.0","result":1,"id":1}' ] || { echo "createProject: $out"; return 1; }
    kb -u admin:admin -d '{"jsonrpc":"2.0","method":"getAllProjects","id":2}' "$url/jsonrpc.php" | grep -q '"name":"zigsaw"' || { echo "no project"; return 1; }
    for p in /app/common.php /vendor/autoload.php /libs/picodb/lib/PicoDb/Database.php /cli; do
        out=$(kb -o /dev/null -w '%{http_code}' "$url$p")
        [ "$out" = 403 ] || { echo "$p: $out"; return 1; }
    done
    [ "$(kb -o /dev/null -w '%{http_code}' "$url/assets/js/app.min.js")" = 200 ] || { echo "no app.min.js"; return 1; }
}
kanboard_start soft
check "Kanboard logs in, creates a project through its API, and keeps its code from the web, under soft" kanboard_serves
kanboard_stop
# In an AppContainer, Kanboard listens on an internal port, and zigsaw's
# relay publishes it; the address it gives is the published one.
kanboard_published() {
    grep -q "Kanboard is at http://127.0.0.1:$kanboard_port/ " "$work\\kanboard.out" || { cat "$work\\kanboard.out"; return 1; }
    kanboard_serves
}
kanboard_start appcontainer
check "and under appcontainer, through the service zigsaw publishes" kanboard_published
kanboard_stop
kanboard_data() {
    local out
    out=$(qd_run z --entrypoint='${php}\php.exe' $KANBOARD "$(deploy_of $KANBOARD)\\kanboard\\cli" db:version 2>&1 | tr -d '\r')
    grep -qE '^Current version: [0-9]+' <<<"$out" || { echo "kanboard-cli: $out"; return 1; }
    [ -f "$QUASIDOCK_HOME\\data\\$KANBOARD\\db.sqlite" ] || { echo "no db.sqlite in its data"; return 1; }
}
check "kanboard-cli makes its database in the app's data" kanboard_data
# PHP's is_writable() is false at low integrity, even for what the run may
# write: it leaves the integrity label out of its access check, and
# Kanboard refuses to start (docs/gaps.md).
kanboard_low() {
    kb "http://127.0.0.1:$kanboard_port/" | grep -q 'must be writeable by your web server user'
}
kanboard_start low
check "but under low it refuses its data directory, which PHP takes for read-only (known gap)" kanboard_low
kanboard_stop

for id in "${installed[@]}"; do qd_remove_app z "$id" >/dev/null 2>&1; done
rm -rf "$work"
echo
echo "$failures check(s) failed."
exit $((failures > 0))
