#!/bin/sh
# Keep the module set explicit: upstream's ENABLE_MODULES is a minimum,
# so setting it to O alone still builds the modules with A/I defaults.
set -eu
set --
for module in $(sed -n 's|.*"modules.*/mod_\([^+]*\)+.*|\1|p' CMakeLists.txt | tr '[:lower:]' '[:upper:]'); do
    set -- "$@" "-DENABLE_$module=O"
done
for module in AUTHZ_CORE AUTHZ_HOST LOG_CONFIG MIME DIR ALIAS ENV SETENVIF FILTER REWRITE EXPIRES HEADERS DEFLATE PROXY PROXY_HTTP PROXY_FCGI PROXY_WSTUNNEL SSL SOCACHE_SHMCB; do
    set -- "$@" "-DENABLE_$module=I"
done
cmake -S . -B out -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$PREFIX" \
    -DINSTALL_PDB=OFF -DINSTALL_MANUAL=OFF \
    -DAPR_LIBRARIES='B:/prefix/lib/libapr-1.dll.a;B:/prefix/lib/libaprutil-1.dll.a' \
    -DPCRE_LIBRARIES=B:/prefix/lib/libpcre2-8.a '-DPCRE_CFLAGS=-DHAVE_PCRE2 -DPCRE2_STATIC' \
    -DZLIB_LIBRARY=B:/prefix/lib/libz.a -DOPENSSL_USE_STATIC_LIBS=ON \
    '-DEXTRA_LIBS=advapi32;crypt32;gdi32;ws2_32' "$@"
cmake --build out
cmake --install out
# No configured file may embed the build drive in the distributable runtime.
rm -f "$PREFIX/conf/httpd.conf" "$PREFIX"/conf/extra/*.conf
mkdir -p "$PREFIX/conf/extra" "$PREFIX/htdocs"
cp docs/conf/mime.types "$PREFIX/conf/mime.types"
