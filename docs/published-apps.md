# Published apps

Sixty-seven recipes in [recipes/](../recipes) build into published OCI images in
`ghcr.io/quasidock/quasidock`, listed in dependency order in
[published-recipes.txt](../scripts/published-recipes.txt). `quasidock search`
queries the published catalog, and `quasidock run --rm <id>` pulls an image on
demand, verifies its `cosign` signature against
[quasidock.pub](../internal/trust/quasidock.pub), deploys its layers, and runs
its entrypoint. See [publishing.md](publishing.md) for how images and the
catalog are published, and [recipes.md](recipes.md) for the recipe format.

## Published images

| Image | Version | Entrypoint / commands | What it is |
|---|---|---|---|
| [`busybox`](../recipes/busybox.json) | `FRP-6075-g169694ebd` | `busybox.exe` | BusyBox for Windows (`sh` and POSIX utilities); build shell for most recipes |
| [`mingit`](../recipes/mingit.json) | `2.56.0.windows.1` | `cmd/git.exe` (`cmd/` in `path`) | MinGit (minimal Git for Windows) |
| [`node`](../recipes/node.json) | `24.22.2` | `node.exe` (`npm` in root; `${data}\npm` in `path`) | Node.js runtime and npm (`soft` sandbox) |
| [`prettier`](../recipes/prettier.json) | `3.9.10` | `${node}\node.exe ${app}\bin\prettier.cjs` | Code formatter (`from: node`) |
| [`python`](../recipes/python.json) | `3.14.10` | `python.exe` | Embeddable Python 3.14 (`soft` sandbox) |
| [`zig`](../recipes/zig.json) | `0.17.0` | `zig.exe` | Zig compiler, linker, and C/C++ toolchain (`soft` sandbox) |
| [`sqlite`](../recipes/sqlite.json) | `3.53.4` | `bin/sqlite3.exe` | SQLite CLI shell |
| [`rust`](../recipes/rust.json) | `1.99.0` | `bin/cargo.exe` (`rustc` in `bin/`, `${data}\cargo\bin` in `path`) | Rust toolchain targeting `x86_64-pc-windows-gnu` (`soft` sandbox) |
| [`ripgrep`](../recipes/ripgrep.json) | `15.2.0` | `rg.exe` | Fast regex file search |
| [`bat`](../recipes/bat.json) | `0.26.1` | `bat.exe` | Syntax-highlighted file viewer |
| [`go`](../recipes/go.json) | `1.27.3` | `bin/go.exe` (`${data}\go\bin` in `path`) | Go compiler and toolchain (`soft` sandbox) |
| [`fzf`](../recipes/fzf.json) | `0.74.5` | `fzf.exe` | Command-line fuzzy finder |
| [`zot`](../recipes/zot.json) | `2.1.23` | `zot.exe` | OCI image registry server (minimal build) |
| [`oras`](../recipes/oras.json) | `1.3.6` | `oras.exe` | Push and pull files in OCI registries |
| [`skopeo`](../recipes/skopeo.json) | `1.24.1` | `skopeo.exe` | Inspect and copy OCI images |
| [`cosign`](../recipes/cosign.json) | `3.1.4` | `cosign.exe` | Sign and verify OCI images and artifacts |
| [`cmake`](../recipes/cmake.json) | `4.4.4` | `bin/cmake.exe` (`ninja` in `bin/`) | CMake and Ninja with `CMAKE_GENERATOR=Ninja` (`soft` sandbox) |
| [`zstd`](../recipes/zstd.json) | `1.5.7` | `bin/zstd.exe` | Zstandard compression CLI |
| [`meson`](../recipes/meson.json) | `1.12.1` | `python.exe ${app}\meson\meson.py` | Meson with embedded Python, Ninja, and `pkgconf` (`soft` sandbox) |
| [`zls`](../recipes/zls.json) | `0.17.0` | `bin\zls.exe` (`${zig}` in `path`) | Zig Language Server (`from: zig`) |
| [`perl`](../recipes/perl.json) | `5.42.3.1` | `perl/bin/perl.exe` (`perl/bin/` in `path`) | Strawberry Perl portable distribution |
| [`cargo-c`](../recipes/cargo-c.json) | `0.10.25` | `bin/cargo-cbuild.exe` (`cargo-cinstall.exe` in `bin/`) | Cargo C-ABI build and install subcommands (`soft` sandbox) |
| [`llvm-mingw`](../recipes/llvm-mingw.json) | `20261020` | `bin/clang.exe` (`bin/` in `path`) | LLVM 23.1.4 Clang, `lld`, and mingw-w64 UCRT (`estargz`, `soft` sandbox) |
| [`gettext`](../recipes/gettext.json) | `1.0` | `bin/gettext.exe` (`msgfmt` in `bin/`) | GNU `gettext` and `libiconv` tools |
| [`gtk4-sdk-base`](../recipes/gtk4-sdk-base.json) | `4.24.1` | `bin/gtk4-demo.exe` (`bin/` in `path`, `g-ir-scanner.exe`) | First half of `gtk4-sdk`: GLib, Pango, Cairo, GTK 4 (`from: meson`, `llvm-mingw`, `busybox`) |
| [`gtk4-sdk`](../recipes/gtk4-sdk.json) | `4.24.1` | `bin/gtk4-demo.exe` (`bin/`, `rsvg-convert.exe`, `msgfmt` from SDK) | Full GTK 4 and libadwaita SDK (`from: meson`, `llvm-mingw`, `busybox`; `soft` sandbox) |
| [`gtk4`](../recipes/gtk4.json) | `4.24.1` | `bin/gtk4-demo.exe` (`gtk4-widget-factory`, `adwaita-1-demo`, `pango-view`, `pango-list`, `gio` in `bin/`) | GTK 4 and libadwaita runtime stripped from `gtk4-sdk` |
| [`gnome-text-editor`](../recipes/gnome-text-editor.json) | `51.0` | `bin/gnome-text-editor.exe` (`bin/` in `path`) | GNOME Text Editor (`from: gtk4`) |
| [`rnote`](../recipes/rnote.json) | `0.14.2` | `bin/rnote.exe` (`rnote-cli` in `bin/`) | Vector sketching and handwritten notes (`from: gtk4`) |
| [`gtk3-sdk`](../recipes/gtk3-sdk.json) | `3.24.52` | `about.bat` (`bin/` in `path`) | GTK 3 and ATK headers and typelibs for `gtk4-sdk` |
| [`gtk3`](../recipes/gtk3.json) | `3.24.52` | `about.bat` (`bin/` in `path`) | GTK 3 and ATK runtime stripped from `gtk3-sdk` |
| [`gtk3demo`](../recipes/gtk3demo.json) | `3.24.52` | `${gtk3}/bin/gtk3-demo.exe` (`gtk3-widget-factory` in `gtk3` `bin/`) | GTK 3 demo and widget factory (`from: gtk4`, `gtk3`) |
| [`gnumeric`](../recipes/gnumeric.json) | `1.12.62` | `bin/gnumeric.exe` (`ssconvert` in `bin/`) | Gnumeric spreadsheet and `ssconvert` (`from: gtk4`, `gtk3`) |
| [`pygobject-sdk`](../recipes/pygobject-sdk.json) | `3.58.1` | `python.exe` (`python3.exe` in root) | Python 3.14, PyGObject, and pycairo headers for `gtk4-sdk` |
| [`pygobject`](../recipes/pygobject.json) | `3.58.1` | `python.exe` (`python3.exe` in root) | Python 3.14, PyGObject, and pycairo runtime stripped from `pygobject-sdk` |
| [`meld`](../recipes/meld.json) | `3.24.1` | `${pygobject}\pythonw.exe ${app}\bin\meld` (`git` from `mingit`) | Visual file and folder diff/merge (`from: gtk4`, `gtk3`, `pygobject`, `mingit`) |
| [`gaphor`](../recipes/gaphor.json) | `3.3.2` | `${pygobject}\pythonw.exe -m gaphor` (`dot.exe` in `bin/`) | UML, SysML, C4, and RAAML modeling (`from: gtk4`, `pygobject`) |
| [`7zip`](../recipes/7zip.json) | `26.05` | `7z.exe` | 7-Zip file archiver |
| [`notepad-plus-plus`](../recipes/notepad-plus-plus.json) | `8.9.8.1` | `notepad++.exe` | Notepad++ source code editor |
| [`sumatrapdf`](../recipes/sumatrapdf.json) | `3.6.1` | `SumatraPDF-3.6.1-64.exe` | Reader for PDF, EPUB, XPS, DjVu, and CHM |
| [`vlc`](../recipes/vlc.json) | `3.0.24` | `vlc.exe` (`cmd`: `--no-qt-privacy-ask`, `--no-qt-updates-notif`) | VLC media player |
| [`php-sdk`](../recipes/php-sdk.json) | `8.5.12-1` | `php.exe` | PHP 8.5 NTS SDK with `dev\zigsaw\php-ext.php` (`from: zig`, `busybox`, `node`; `soft` sandbox) |
| [`php`](../recipes/php.json) | `8.5.12-1` | `php.exe` | PHP 8.5 NTS runtime stripped from `php-sdk` |
| [`composer`](../recipes/composer.json) | `2.10.3` | `${php}\php.exe ${app}\composer.phar` | Composer PHP dependency manager (`from: php`) |
| [`kanboard`](../recipes/kanboard.json) | `1.2.57` | `${php}\php.exe ${app}\serve.php` | Kanboard web app (`from: php`, `http` on `8471`, `soft` sandbox; `low` unsupported) |
| [`java-sdk`](../recipes/java-sdk.json) | `25.0.5_8` | `bin/java.exe` (`javac` in `bin/`) | Eclipse Temurin OpenJDK 25 JDK (`soft` sandbox) |
| [`java`](../recipes/java.json) | `25.0.5_8` | `bin/java.exe` | Eclipse Temurin OpenJDK 25 JRE |
| [`maven`](../recipes/maven.json) | `3.10.0` | `${java}\bin\java.exe ... Launcher` | Apache Maven build tool (`from: java-sdk`, `soft` sandbox) |
| [`gradle`](../recipes/gradle.json) | `9.8.1` | `${java}\bin\java.exe ... gradle-gradle-cli-main-9.8.1.jar` | Gradle build tool with offline vendoring helpers (`from: java-sdk`, `soft` sandbox) |
| [`pdfbox`](../recipes/pdfbox.json) | `3.0.8` | `${java}\bin\java.exe -jar ${app}\pdfbox-app.jar` | Apache PDFBox CLI and PDF debugger (`from: java`) |
| [`plantuml`](../recipes/plantuml.json) | `1.2026.9` | `${java}\bin\java.exe -jar ${app}\plantuml.jar` | PlantUML diagram CLI and GUI (`from: java`) |
| [`logisim-evolution`](../recipes/logisim-evolution.json) | `5.0.0` | `${java}\bin\java.exe ... -jar ${app}\logisim-evolution.jar` | Digital logic circuit simulator (`from: java`) |
| [`scenebuilder`](../recipes/scenebuilder.json) | `26.0.0` | `${java}\bin\javaw.exe ... SceneBuilderApp` | Scene Builder JavaFX UI designer (`from: java-sdk`) |
| [`qt6-sdk-base`](../recipes/qt6-sdk-base.json) | `6.12.0` | `bin/qtpaths.exe` (`bin/` in `path`) | First half of `qt6-sdk`: `qtbase`, `qtsvg`, `qtimageformats`, `qtshadertools` (`from: cmake`, `llvm-mingw`, `busybox`, `estargz`) |
| [`qt6-sdk`](../recipes/qt6-sdk.json) | `6.12.0` | `bin/qtpaths.exe` (`designer.exe` in `bin/`) | Full Qt 6 SDK with Widgets, QML, and Qt Quick (`from: cmake`, `llvm-mingw`, `busybox`, `estargz`, `soft` sandbox) |
| [`qt6`](../recipes/qt6.json) | `6.12.0` | `bin/designer.exe` (`linguist`, `qml`, `qtdiag` in `bin/`) | Qt 6 runtime stripped from `qt6-sdk` (`estargz`) |
| [`flameshot`](../recipes/flameshot.json) | `14.0.0` | `bin/flameshot.exe` (`bin/` in `path`) | Screenshot and annotation app (`from: qt6`) |
| [`notes`](../recipes/notes.json) | `2.3.1` | `bin/Notes.exe` (`bin/` in `path`) | Markdown notes and to-do board (`from: qt6`) |
| [`ghostscript`](../recipes/ghostscript.json) | `10.08.0` | `bin/gswin64c.exe` (`bin/` in `path`) | Ghostscript PostScript and PDF interpreter |
| [`texlive`](../recipes/texlive.json) | `2026.80721` | `bin/windows/latexmk.exe` (`pdflatex`, `xelatex`, `lualatex`, `luatex`, `pdftex` in `bin/windows/`) | TeX Live small scheme (`from: perl`, `ghostscript`, `estargz`) |
| [`gdal-sdk`](../recipes/gdal-sdk.json) | `3.13.3` | `bin/gdalinfo.exe` (`bin/` in `path`) | GDAL SDK with static GEOS, PROJ, SQLite, json-c, Expat, libiconv |
| [`gdal`](../recipes/gdal.json) | `3.13.3` | `bin/gdal.exe` (`gdalinfo`, `ogr2ogr`, `gdal_translate`, `gdalwarp` in `bin/`) | GDAL raster and vector tools stripped from `gdal-sdk` |
| [`make`](../recipes/make.json) | `4.4.1` | `bin/make.exe` (`sh.exe` in `bin/`) | GNU Make with BusyBox `sh` (`soft` sandbox) |
| [`postgresql-sdk`](../recipes/postgresql-sdk.json) | `18.6` | `bin/pg_config.exe` (`bin/` in `path`) | PostgreSQL 18 SDK with `libpq` and `pgxs` |
| [`postgresql`](../recipes/postgresql.json) | `18.6` | `bin/pg-server.exe` (`psql`, `pg_ctl`, `initdb`, `pg_dump`, `pg_restore`, `pg_isready` in `bin/`) | PostgreSQL 18 server and clients with PL/Perl and PL/Python (`from: perl`, `python`, port `5432`) |
| [`osm2pgsql`](../recipes/osm2pgsql.json) | `2.3.1` | `bin/osm2pgsql.exe` (`osm2pgsql-expire` in `bin/`) | OpenStreetMap-to-PostGIS importer (`uses: postgresql` on `5432`) |
| [`vscodium`](../recipes/vscodium.json) | `1.143.17232` | `VSCodium.exe` (`codium` in `bin/`, `git` from `mingit`, `host: true`) | VSCodium editor (`from: mingit`, `estargz`, `soft` sandbox; `low`/`appcontainer` unsupported) |

One additional recipe, [recipes/gtk4-msvc.json](../recipes/gtk4-msvc.json)
(`4.24.1`), is neither in `published-recipes.txt` nor in `recipes/no-publish/`:
it declares `"host": ["msvc"]` so maintainers can build GTK 4 with Visual Studio
locally when running [tests/gtk.sh](../tests/gtk.sh).

### Running images, `from` base images, and commands

Without a command or `--entrypoint`, `quasidock run` executes the image's
`entrypoint` followed by any default `cmd` (such as `vlc`'s suppression flags).
Passing `COMMAND [ARG...]` after `IMAGE` looks up another executable in the
deployed image root, the image's `path` entries (plus those of its `from` base
images), and `System32` — **unless** `COMMAND` matches the image `id` (or the
entrypoint executable's stem when `entrypoint` has leading arguments), in which
case Quasidock keeps `entrypoint` and replaces `cmd` with `[ARG...]`. Using
`--entrypoint=COMMAND` has the same overriding effect (or preserves a
multi-element `entrypoint` when set to the image `id`), and is useful for
secondary commands or switching a GUI `pythonw.exe` entrypoint to `python.exe`:

```powershell
quasidock run --rm 7zip 7z x archive.7z
quasidock run --rm prettier prettier --version
quasidock run --rm pdfbox pdfbox export:text -console -i doc.pdf
quasidock run --rm gnumeric ssconvert sheet.csv sheet.xlsx
quasidock run --rm --entrypoint=rnote-cli rnote --version
quasidock run --rm --entrypoint='${pygobject}\python.exe' gaphor -m gaphor --version
```

Child images reuse shared stacks through `from` base images instead of embedding
separate copies: `node` backs `prettier`; `php` backs `composer` and `kanboard`;
`java` backs `pdfbox`, `plantuml`, and `logisim-evolution`; `java-sdk` backs
`maven`, `gradle`, and `scenebuilder`; `gtk4` (plus `gtk3`, `pygobject`, and
`mingit` where needed) backs `gnome-text-editor`, `rnote`, `gtk3demo`,
`gnumeric`, `meld`, and `gaphor`; `qt6` backs `flameshot` and `notes`; `perl`
and `ghostscript` back `texlive`; `perl` and `python` back `postgresql`; `zig`
backs `zls`; `mingit` backs `vscodium`; and `cmake`, `meson`, `llvm-mingw`, and
`busybox` back the GTK and Qt SDK images.

Custom extension images can still be composed with `from` and built using the
published SDKs (`php-sdk` ships `dev\zigsaw\php-ext.php` and `zig` for PHP
extensions loaded via `PHP_INI_SCAN_DIR`, while `postgresql-sdk` ships
`pg_config` and `pgxs` for `make` and `llvm-mingw`); standalone extension
images are no longer published separately. Note that `php` bundles a `php.ini`
with `curl`, `fileinfo`, `gd`, `intl`, `mbstring`, `openssl`, `pdo_sqlite`,
`sqlite3`, `sodium`, and `zip` enabled.

Six large toolchain and UI images (`llvm-mingw`, `qt6-sdk-base`, `qt6-sdk`,
`qt6`, `texlive`, and `vscodium`) set `"layer": "estargz"` so Quasidock can
pull and materialize files lazily; the rest use gzip-compressed OCI layers. See
[layer-formats.md](layer-formats.md) for layout details.

### Per-container data and named volumes

Every `quasidock run` allocates an isolated `containers/<id>/data` directory
and its own `HKCU` registry hive under `containers/<id>/registry/user.dat`,
both removed on exit when `--rm` is passed. Images whose environment points to
`${data}` — `node` (`NPM_CONFIG_PREFIX`), `rust` (`CARGO_HOME`), `go` (`GOPATH`),
`java` and `java-sdk` (`user.home`), `texlive` (`TEXMFVAR`, `TEXMFCONFIG`,
`TEXMFHOME`), `kanboard` (`DATA_DIR`, `PLUGINS_DIR`), and `postgresql` (`PGDATA`,
`PGPASSFILE`) — keep state private to each container unless you reuse a named
container, bind a host folder (`--mount type=bind`), or mount a named volume
(`volumes/<name>/data`):

```powershell
quasidock run --rm --port=postgresql=5432 -v 'pgdata:${data}' postgresql
quasidock run --rm --port=postgresql=5432 -v 'pgdata:${data}' postgresql psql -XAt -c 'select version();'
```

Quote `-v 'pgdata:${data}'` in PowerShell so `${data}` is not expanded before
`quasidock` receives it. `postgresql` exposes its `postgresql` service on port
`5432` (consumed by `osm2pgsql` via `uses` / `--port=postgresql=...`), and
`kanboard` serves `http` on port `8471` (`KANBOARD_LISTEN`, `KANBOARD_URL`).
Note that `kanboard` refuses to run under `--sandbox=low` because PHP's
`is_writable()` reports false at low integrity; it defaults to `soft`. See
[networking.md](networking.md) and
[README.md](../README.md#persistent-storage).

### VSCodium and `quasidock-host`

`vscodium` layers `mingit` via `from` so both VSCodium's Git extension and its
integrated terminal find `git` on `PATH`, strips `"updateUrl"` and
`setAsDefaultProtocolClient` so it does not check for updates or alter `HKCU`,
and exposes `codium` (`bin\codium.cmd`) alongside `VSCodium.exe`. It defaults
to `--sandbox=soft`: Windows refuses Chromium's sandbox a window station at
`low` and in `appcontainer` (exit code `3`; `--no-sandbox` bypasses that at `low`
at the cost of disabling Chromium's sandbox, while Node still fails path
resolution in `appcontainer`).

`vscodium` also sets `"host": true` in `permissions` (the recipe equivalent of
`--share=host`). Under `soft` and `low`, Quasidock appends the store's `host`
directory containing `quasidock-host.exe`
([cmd/quasidock-host](../cmd/quasidock-host)) to `PATH` (in `appcontainer` it is
omitted with a note; `--unshare=host` disables it). Inside the container
terminal, `quasidock-host <program> [args...]` searches the user's real host
`PATH` and `PATHEXT` (running `.cmd`/`.bat` files through `cmd.exe`) and launches
host tools under the active container sandbox and rights.

## Apps you build yourself

Recipes under [recipes/no-publish/](../recipes/no-publish) are excluded from
publishing (`scripts/publish.sh` rejects them and `scripts/rebuild.sh` leaves
them alone) because their upstream licenses do not permit redistributing built
images. Build them locally with `quasidock build`:

| Image | Version | Entrypoint | Notes |
|---|---|---|---|
| [`dotnet-sdk`](../recipes/no-publish/dotnet-sdk.json) | `10.0.402` | `dotnet.exe` | .NET SDK, C#, F#, MSBuild, NuGet; `${data}\dotnet\.dotnet\tools` in `path`; `DOTNET_CLI_HOME=${data}\dotnet` (`soft` sandbox) |
| [`dotnet`](../recipes/no-publish/dotnet.json) | `10.0.13` | `dotnet.exe` | .NET, ASP.NET Core, and Windows Desktop runtime stripped from `dotnet-sdk` |
| [`pinta`](../recipes/no-publish/pinta.json) | `3.1.2` | `bin/Pinta.exe` | Pinta image editor (`from: dotnet`, `gtk4`; `sdk: dotnet-sdk`, `gettext`, `busybox`) |
| [`files`](../recipes/no-publish/files.json) | `4.3` | `bin/Files.exe` | Files manager (`from: dotnet`, `sdk: dotnet-sdk`, `busybox`; keeps `"Files - Zigsaw build"` recipe `name`; `soft` sandbox — Windows storage APIs hide drives from unpackaged apps under `low` and `appcontainer`) |
| [`ungoogled-chromium`](../recipes/no-publish/ungoogled-chromium.json) | `156.0.8078.73-1.1` | `chrome.exe` | ungoogled-chromium (`soft` sandbox; `HKCU\Software\Chromium` goes to per-container registry hive; Chromium's sandbox fails at `low` and in `appcontainer` with exit `3` unless `--no-sandbox` disables it) |

```powershell
quasidock build recipes\no-publish\dotnet-sdk.json
quasidock build recipes\no-publish\dotnet.json
quasidock build recipes\no-publish\pinta.json
quasidock build recipes\no-publish\files.json
quasidock build recipes\no-publish\ungoogled-chromium.json
```

`quasidock build` resolves `from` and `sdk` pins from the local store first and
pulls published dependencies (`gtk4`, `gettext`, `busybox`) from the configured
registry if not yet cached. Mount a named volume at `${data}`
(`-v 'dotnet-cache:${data}'`) when running `dotnet-sdk` with `--rm` if you want
to keep NuGet packages and installed tools across runs.

## Reproducibility

`scripts/check-pins.sh` checks every recipe in `scripts/published-recipes.txt`
anonymously against `ghcr.io/quasidock/quasidock` (or `QUASIDOCK_REGISTRY`): it verifies
that every `from` and `sdk` pin resolves to a manifest, every recipe's
`id:version` tag is published, warns if a pinned `from` image has layers that
differ from its published version, and fails on `ghcr.io/quasidock/quasidock` if any
published image lacks a `.sig` signature tag.

The [Reproduce](../.github/workflows/reproduce.yml) workflow on
`quasidock/quasidock` runs on pushes to `main`, pull requests, manual dispatch,
and Mondays at 03:17 UTC:

- **`image-cli`** (`windows-2025`): runs Go integration tests across `store`,
  `cli`, `environment`, `mounts`, `grants`, `hkcu`, `run`, `publishing`, and
  `cmd/quasidock-publish`, plus `tests/publish.sh` offline.
- **`pins`** (`ubuntu-24.04`): runs `scripts/check-pins.sh`,
  [tests/upstream.sh](../tests/upstream.sh), and
  [tests/rebuild.sh](../tests/rebuild.sh).
- **`groups`** (`ubuntu-24.04`): runs
  [scripts/ci-groups.sh](../scripts/ci-groups.sh) and `tests/build.sh --groups`
  to emit CI matrix groups (push/PR runs skip the heavy GTK, Qt, TeX Live, and
  GDAL groups of [published-groups.txt](../scripts/published-groups.txt) when
  unchanged; weekly and manual runs build all groups).
- **`published`** (`windows-2025`, matrix): seeds BusyBox sources via
  `quasidock-publish seed-busybox`, rebuilds each group's recipes cleanly, and
  runs [tests/published.sh](../tests/published.sh) to check rebuilt layer
  digests against the published registry manifests, smoke-test entrypoints,
  verify DLL imports, and check `quasidock search` catalog results.
- **`suites`** (`windows-2025`): seeds a store with published images via
  `tests/seed-store.sh` and runs `store.sh`, `sandbox.sh`, `batch.sh`,
  `mounts.sh`, `ctrlc.sh`, `network.sh`, `reach.sh`, `install.sh`, the local
  `zot` registry suites (`registry.sh`, `lazy.sh`, `signing.sh`, `oci.sh`),
  `zls.sh`, `texlive.sh`, `gdal.sh`, `postgresql.sh`, `osm2pgsql.sh`,
  `certs.sh`, and `matrix.sh`. Interactive GUI and `no-publish` suites
  (`gtk.sh`, `gtk3.sh`, `qt.sh`, `java.sh`, `php.sh`, `files.sh`, `chromium.sh`)
  are run manually outside CI.
- **`standard-user`** (`windows-2025`): runs `tests/standard-user.sh` under a
  throwaway non-administrator account.
- **`build`** (`windows-2025`, matrix): runs `tests/build.sh` groups plus
  `internal/version/version.go` validation and `go test ./...` on `base`.

For publishing images and regenerating the catalog (`publish.yml` and
`scripts/publish.sh`), see [publishing.md](publishing.md). The
[Release](../.github/workflows/release.yml) workflow publishes Quasidock CLI
release archives on `v*` tags after running `go test ./...`,
`scripts/check-pins.sh`, `scripts/package.sh`, `tests/install.sh`, and Defender
scans.

## What's out of date

[scripts/upstream.txt](../scripts/upstream.txt) pairs each recipe's download
sources with an upstream release source and optional OSV package rule.
[scripts/check-upstream.sh](../scripts/check-upstream.sh) checks both upstream
release pages and the OSV vulnerability API in batches:

- Default output writes Markdown sections for `Known advisories`, `OSV's records
  are thin here`, `Broken`, `Updates within a series`, `Only a new series`, and
  `Unreachable`.
- `--all` adds `Current`, `Not checked`, `No advisories listed`, and `OSV has no
  record of these`.
- `--coverage` verifies `upstream.txt` matches the recipe files (note that
  `upstream.txt` still contains legacy `php-deps/*` lines from removed PHP
  extension recipes; `tests/upstream.sh` checks script behavior and recipe
  coverage).
- `--new-advisories <old> <new>` extracts advisories present in `<new>` that were
  absent in `<old>`.

The [Upstream](../.github/workflows/upstream.yml) workflow runs Mondays at 04:47
UTC and on `workflow_dispatch` (`ubuntu-24.04`, `issues: write`). It runs
`scripts/check-upstream.sh`, keeps a single open issue labelled `upstream-report`
updated with the full report, posts a comment via `--new-advisories` whenever
new advisories appear, and fails the workflow if `check-upstream.sh` exits
non-zero on broken rows or coverage mismatches.

### Reading the advisories

OSV matches on upstream product/version ranges rather than build options or
file cleanup rules: a listed CVE may affect a module stripped by `cleanup`, a
disabled feature, or a build-only `sdk` input rather than published runtime
binaries. Always check a recipe's `stages`, `cleanup`, and `from`/`sdk` graph
alongside upstream release notes, and pay attention to the `OSV's records are
thin here` and `Not checked` sections where OSV lacks coverage for Windows or
redistributed binaries.

### Rebuilding

[scripts/rebuild.sh](../scripts/rebuild.sh) walks
[published-recipes.txt](../scripts/published-recipes.txt) in topological
dependency order (assigning equal ranks across independent branches using `from`
and `sdk` edges), skips `recipes/no-publish/` (listing them at the end because
their pins must be updated by hand), builds updated recipes and every downstream
dependent, rewrites downstream `from` and `sdk` digest pins in `recipes/*.json`,
and prints the `scripts/publish.sh` command needed to publish the result (see
[publishing.md](publishing.md)):

```powershell
bash scripts/rebuild.sh --plan
bash scripts/rebuild.sh
```

Pass `--plan` to inspect the topological rank order without building, or
`--no-pin` to test builds without rewriting `recipes/*.json` pins.
