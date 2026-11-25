#!/bin/sh
# Libraries are built by earlier stages, using the same native toolchain.
set -eu
trap 'code=$?; if [ "$code" != 0 ] && [ -f objs/autoconf.err ]; then cat objs/autoconf.err >&2; fi' EXIT
sh configure --crossbuild=win32 --with-cc=cc \
    --prefix= --sbin-path=libexec/nginx.exe --conf-path=conf/nginx.conf \
    --pid-path=logs/nginx.pid --error-log-path=logs/error.log \
    --http-log-path=logs/access.log \
    --http-client-body-temp-path=temp/client_body \
    --http-proxy-temp-path=temp/proxy --http-fastcgi-temp-path=temp/fastcgi \
    --http-uwsgi-temp-path=temp/uwsgi --http-scgi-temp-path=temp/scgi \
    --with-cc-opt='-O2 -DFD_SETSIZE=1024 -DPCRE2_STATIC -IB:/prefix/include' \
    --with-ld-opt='-s -LB:/prefix/lib -lcrypt32 -lgdi32 -lws2_32' \
    --with-http_ssl_module --with-http_v2_module --with-pcre-jit
make -j "$(nproc)"
mkdir -p "$PREFIX/bin" "$PREFIX/libexec" "$PREFIX/conf" "$PREFIX/html"
cp objs/nginx.exe "$PREFIX/libexec/nginx.exe"
cp conf/mime.types conf/fastcgi_params conf/fastcgi.conf conf/uwsgi_params conf/scgi_params "$PREFIX/conf/"
cp LICENSE "$PREFIX/licenses/nginx.txt"
cc -O2 -Wall -Wextra -Werror -municode -s quasidock/launcher.c -o "$PREFIX/bin/nginx.exe"
