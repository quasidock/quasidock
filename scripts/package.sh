#!/usr/bin/env bash
# Builds zigsaw and lays out a release's files, as the Release workflow
# (.github/workflows/release.yml) publishes them and scripts/install.ps1
# installs them:
#
#   zigsaw-x86_64-windows.zip   zigsaw.exe, zigsaw-shim.exe, zigsaw-shimw.exe,
#                               zigsaw-relay.exe, zigsaw-host.exe, LICENSE, and Zig's licence
#                               (its standard library is compiled into them)
#   install.ps1                 scripts/install.ps1
#   VERSION                     which release this is
#   SHA256SUMS                  of those three
#   notes.md                   the release's notes: the programs' own hashes
#
#   scripts/package.sh [output directory]     (default: zig-out/release)
#
# Needs Git Bash and Zig. zigsaw.exe is built without debug information
# (-Dstrip), which would name the directory it was built in: the four
# programs are then the same bytes from any checkout of the same commit, with
# the same Zig, so anyone can check a release against a build of their own.
# The zip itself isn't: it records when it was made.

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/zig-out/release}
cd "$root"

rm -rf "$out"
mkdir -p "$out/files"
# Into a prefix of its own: zig-out/bin keeps the build with debug information.
go run "$(cygpath -w "$root/scripts/build.go")" -Dstrip -p "$(cygpath -w "$out/build")"
version=$("$out/build/bin/zigsaw.exe" version | tr -d '\r' | cut -d' ' -f2)
cp "$out/build/bin/zigsaw.exe" "$out/build/bin/zigsaw-shim.exe" "$out/build/bin/zigsaw-shimw.exe" "$out/build/bin/zigsaw-relay.exe" "$out/build/bin/zigsaw-host.exe" "$out/build/bin/zigsaw-notify.exe" LICENSE "$out/files/"
rm -rf "$out/build"
# Go's licence is next to its lib directory.
go_root=$(go env GOROOT | tr -d '\r')
if [ -f "$go_root/LICENSE" ]; then
    cp "$go_root/LICENSE" "$out/files/LICENSE-go.txt"
fi

powershell -NoProfile -Command "Compress-Archive -Path '$(cygpath -w "$out/files")\\*' -DestinationPath '$(cygpath -w "$out")\\zigsaw-x86_64-windows.zip'"
cp scripts/install.ps1 "$out/"
# Which release this is, for `zigsaw updates` and `zigsaw upgrade` to read.
printf '%s\n' "$version" >"$out/VERSION"

sum() { sha256sum <"$1" | cut -d' ' -f1; }
(
    cd "$out"
    for f in zigsaw-x86_64-windows.zip install.ps1 VERSION; do printf '%s  %s\n' "$(sum "$f")" "$f"; done >SHA256SUMS
    {
        printf 'Install or upgrade, in PowerShell:\n\n'
        printf '```powershell\nirm https://github.com/zigsaw-io/zigsaw/releases/download/v%s/install.ps1 | iex\n```\n\n' "$version"
        printf 'Built with %s by `go run ./scripts/build.go -Dstrip`. A build of this tag has the same programs:\n\n' "$(go version | tr -d '\r')"
        printf '| Program | SHA-256 |\n|---|---|\n'
        for f in zigsaw.exe zigsaw-shim.exe zigsaw-shimw.exe zigsaw-relay.exe zigsaw-host.exe zigsaw-notify.exe; do printf '| `%s` | `%s` |\n' "$f" "$(sum "files/$f")"; done
    } >notes.md
)
rm -rf "$out/files"

echo "zigsaw $version packaged in $(cygpath -w "$out"):"
cat "$out/SHA256SUMS"
