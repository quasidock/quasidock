#!/usr/bin/env bash
# Build erlang first in the Quasidock store named by ERLANG_TEST_HOME.
set -euo pipefail
export MSYS_NO_PATHCONV=1
root=$(cd "$(dirname "$0")/.." && pwd)
source "$root/scripts/images.sh"
cli=${1:-${QUASIDOCK:-$root/out/bin/quasidock.exe}}
export QUASIDOCK_HOME=${ERLANG_TEST_HOME:-${RABBITMQ_HOME:-${QUASIDOCK_HOME:-}}}
image=${ERLANG_IMAGE:-erlang}
qd_cached "$cli" "$image" || { echo 'Build recipes/erlang.json in ERLANG_TEST_HOME first.' >&2; exit 2; }
scratch=$(realpath "${TMPDIR:-/tmp}")
work=$(mktemp -d "$scratch/quasidock-erlang.XXXXXX")
trap 'case "$work" in "$scratch"/quasidock-erlang.*) rm -rf -- "$work";; esac' EXIT
cd "$work"
cat >hello.erl <<'ERL'
-module(hello).
-export([main/0]).
main() -> ok = application:ensure_started(crypto),
          {ok, {127,0,0,1}} = inet:getaddr("localhost", inet),
          {ok, _} = application:ensure_all_started(os_mon),
          32 = byte_size(crypto:strong_rand_bytes(32)),
          {ok, _} = application:ensure_all_started(ssl),
          erlang:display(erlang_ok), halt().
ERL
cat >hello.escript <<'ERL'
#!/usr/bin/env escript
main(_) -> erlang:display(escript_ok).
ERL
bind=$(cygpath -w "$work")
for sandbox in low soft appcontainer; do
    "$cli" run --rm --sandbox="$sandbox" -v "$bind:\${data}" --entrypoint=erlc.exe "$image" -o "$bind" "$bind/hello.erl"
    "$cli" run --rm --sandbox="$sandbox" -v "$bind:\${data}" "$image" erl.exe -noshell -pa "$bind" -s hello main | grep -q erlang_ok
    "$cli" run --rm --sandbox="$sandbox" -v "$bind:\${data}" --entrypoint=escript.exe "$image" "$bind/hello.escript" | grep -q escript_ok
    echo "ok    Erlang compiler, DNS, OS monitor, crypto, TLS and escript in $sandbox"
done
