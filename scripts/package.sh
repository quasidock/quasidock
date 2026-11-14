#!/usr/bin/env bash
# Builds quasidock and lays out a release's files, as the Release workflow
# (.github/workflows/release.yml) publishes them and scripts/install.ps1
# installs them:
#
#   quasidock-x86_64-windows.zip quasidock.exe, quasidock-relay.exe,
#                               quasidock-host.exe, LICENSE, and Go's licence
#   install.ps1                 scripts/install.ps1
#   SHA256SUMS                  of those two
#   notes.md                    the release's notes: the programs' own hashes
#
#   scripts/package.sh [output directory]     (default: out/release)
#
# Needs Git Bash and Go. quasidock.exe is built with --strip
# (-trimpath/-s -w), so a build from any checkout of the same commit with the
# same Go toolchain has matching bytes. The zip itself records timestamps.

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/out/release}
cd "$root"

rm -rf "$out"
mkdir -p "$out/files"
# Into a prefix of its own: out/bin keeps the unstripped build.
go run "$(cygpath -w "$root/scripts/build.go")" --strip -p "$(cygpath -w "$out/build")"
version=$("$out/build/bin/quasidock.exe" version | tr -d '\r' | cut -d' ' -f2)
cp "$out/build/bin/quasidock.exe" "$out/build/bin/quasidock-relay.exe" "$out/build/bin/quasidock-host.exe" LICENSE "$out/files/"
rm -rf "$out/build"
# Go's licence is next to its lib directory.
go_root=$(go env GOROOT | tr -d '\r')
if [ -f "$go_root/LICENSE" ]; then
    cp "$go_root/LICENSE" "$out/files/LICENSE-go.txt"
fi

powershell -NoProfile -Command "Compress-Archive -Path '$(cygpath -w "$out/files")\\*' -DestinationPath '$(cygpath -w "$out")\\quasidock-x86_64-windows.zip'"
cp scripts/install.ps1 "$out/"

sum() { sha256sum <"$1" | cut -d' ' -f1; }
(
    cd "$out"
    for f in quasidock-x86_64-windows.zip install.ps1; do printf '%s  %s\n' "$(sum "$f")" "$f"; done >SHA256SUMS
    {
        printf 'Install or upgrade, in PowerShell:\n\n'
        printf '```powershell\nirm https://github.com/quasidock/quasidock/releases/download/v%s/install.ps1 | iex\n```\n\n' "$version"
        printf 'Built with %s by `go run ./scripts/build.go --strip`. A build of this tag has the same programs:\n\n' "$(go version | tr -d '\r')"
        printf '| Program | SHA-256 |\n|---|---|\n'
        for f in quasidock.exe quasidock-relay.exe quasidock-host.exe; do printf '| `%s` | `%s` |\n' "$f" "$(sum "files/$f")"; done
    } >notes.md
)
rm -rf "$out/files"

echo "quasidock $version packaged in $(cygpath -w "$out"):"
cat "$out/SHA256SUMS"
