#!/bin/sh
set -eu
# The release source distribution contains its complete source dependency tree.
# Use erlang.mk's Windows branch with native paths, without depending on MSYS.
find . -name erlang.mk -exec sed -i 's|core_native_path = $(shell cygpath -m $1)|core_native_path = $1|' '{}' ';'
sed -i 's|core_unix_path = $(shell cygpath $1)|core_unix_path = $1|' deps/rabbit_common/mk/rabbitmq-dist.mk
# GNU awk accepts an unescaped opening brace here; BusyBox requires a literal.
sed -i 's|{ \*vsn|[{] *vsn|g' deps/rabbit_common/mk/rabbitmq-dist.mk
export ERL_COMPILER_OPTIONS=deterministic
export ELIXIR_LIBS=B:/src/rabbitmq/toolchain/elixir/lib
export ELIXIR_BIN=B:/src/rabbitmq/toolchain/elixir/bin/elixir
export ERL_LIBS="$ELIXIR_LIBS"
make PLATFORM=msys2 CC=cc.exe ELIXIR_ERL_EBIN="$ELIXIR_LIBS/elixir/ebin" \
    PYTHON='python.exe -X utf8 B:/src/rabbitmq/quasidock/python-codegen.py' PROJECT_VERSION=4.4.1 \
    ESCRIPT_ZIP='python.exe B:/src/rabbitmq/quasidock/zip-escript.py' \
    dist install-windows-escripts WINDOWS_PREFIX="$PREFIX"
cp -R plugins "$PREFIX/"
cp LICENSE* "$PREFIX/"
mkdir -p "$PREFIX/licenses"
cp toolchain/elixir/LICENSE "$PREFIX/licenses/elixir.txt"
