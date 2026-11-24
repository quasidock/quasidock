#!/bin/sh
# All dependencies are installed by earlier, digest-pinned stages.
set -eu
cmake -S . -B out -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_POLICY_VERSION_MINIMUM=3.5 -DCMAKE_INSTALL_PREFIX="$PREFIX" \
    -DINSTALL_LAYOUT=STANDALONE -DINSTALL_RUNTIME_DEPENDENCIES=OFF \
    -DINSTALL_MYSQLTESTDIR= \
    -DWITH_UNIT_TESTS=OFF -DWITH_EMBEDDED_SERVER=OFF -DWITH_WSREP=OFF \
    -DPLUGIN_ROCKSDB=NO -DPLUGIN_COLUMNSTORE=NO -DPLUGIN_SPIDER=NO -DPLUGIN_CONNECT=NO \
    -DWITH_SSL=system -DOPENSSL_USE_STATIC_LIBS=ON -DOPENSSL_ROOT_DIR="$PREFIX" \
    -DWITH_ZLIB=system -DZLIB_LIBRARY="$PREFIX/lib/libz.a" \
    -DWITH_PCRE=system -DPCRE2_INCLUDE_DIR="$PREFIX/include" \
    -DWITH_LIBFMT=system -DLIBFMT_INCLUDE_DIR="$PREFIX/include" \
    -DBISON_EXECUTABLE=B:/src/flex-bison/win_bison.exe \
    '-DCMAKE_C_FLAGS=-DPCRE2_STATIC -D_WIN32_WINNT=0x0A00 -DNOMINMAX -fms-extensions' \
    '-DCMAKE_CXX_FLAGS=-DPCRE2_STATIC -D_WIN32_WINNT=0x0A00 -DNOMINMAX -fms-extensions' \
    '-DCMAKE_EXE_LINKER_FLAGS=-s -Wl,--no-insert-timestamp' \
    '-DCMAKE_SHARED_LINKER_FLAGS=-s -Wl,--no-insert-timestamp' \
    '-DCMAKE_MODULE_LINKER_FLAGS=-s -Wl,--no-insert-timestamp'
cmake --build out --parallel 8
cmake --install out
cp COPYING "$PREFIX/LICENSE.mariadb"
cp libmariadb/COPYING.LIB "$PREFIX/licenses/connector-c.txt"
