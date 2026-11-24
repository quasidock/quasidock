#!/bin/sh
set -eu
# Windows loads client DLLs next to the executables. Import/static libraries
# remain in lib for downstream builds; plugins remain in lib/plugin.
for dll in "$PREFIX"/lib/*.dll; do
    [ ! -f "$dll" ] || mv "$dll" "$PREFIX/bin/"
done
cp "$PREFIX/lib/libmariadbclient.a" "$PREFIX/lib/libmariadb.a"
mkdir -p "$PREFIX/lib/pkgconfig"
cp libmariadb.pc "$PREFIX/lib/pkgconfig/"
for pc in "$PREFIX"/lib/pkgconfig/*.pc; do
    sed -i 's|B:/prefix|${pcfiledir}/../..|g' "$pc"
done
! grep -rE 'B:/(src|prefix|bin)|[/\\]deploy[/\\][0-9a-f]{64}' \
    "$PREFIX/lib/pkgconfig" "$PREFIX/share/cmake"
test -f "$PREFIX/bin/libmariadb.dll"
test -f "$PREFIX/lib/libserver.dll.a"
test -f "$PREFIX/share/cmake/mariadb-plugin/mariadb-plugin-config.cmake"
