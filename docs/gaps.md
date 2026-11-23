# Known gaps

Known limitations, platform constraints, and incomplete features in Quasidock
0.10.0.

## Runs, sandboxes, and grants

- **Lifecycle signals and attachment.** `start` defaults to background and
  supports `-a`/`-i`; `restart` launches in the background. Attachment supports
  `run -d` and `start` containers only, with one stdin writer and multiple output
  viewers; foreground `run` attachment and automatic restart policies remain
  unsupported. Clients do not follow a replacement run after restart.
  `kill --signal TERM` maps to Ctrl+Break for detached console process groups,
  which applications may ignore. Foreground TERM is rejected to preserve native
  terminal Ctrl+C. KILL ends the Job Object for either mode. Finish active runs
  before upgrading a binary whose supervisor uses the older stop-event protocol.
- **Absolute paths recorded inside container data break if deployments move.**
  A command or launcher that embeds an image's deployment path (such as pip
  entry-point executables pointing at `python.exe`) stops working once that
  deployment is replaced; `npm` and `cargo` shims do not hardcode deployment
  paths. Converted Python Dockerfiles avoid pip's launchers for RUN and startup
  console commands by dispatching through installed entry-point metadata.
- **Batch delayed expansion.** A `.bat` or `.cmd` file that turns on delayed
  expansion itself (`setlocal EnableDelayedExpansion`) expands `!var!` in its
  own arguments.
- **Node's `--preserve-symlinks` affects every sandbox.** `recipes/node.json`
  sets `NODE_OPTIONS=--preserve-symlinks --preserve-symlinks-main` so Node
  works in AppContainers; symlinked packages (`npm link`, workspaces, `pnpm`)
  then resolve dependencies relative to the link across all sandboxes.
- **Standing host grants outlast individual runs ([how long grants last](how-it-works.md#how-long-grants-last)).**
  Under the default `low` sandbox, directories granted via `cwd` or
  `--filesystem` remain labeled Low Integrity (writable by any low-integrity
  process) and stay listed in `grants\<id>.txt` until `quasidock rmi` (when
  removing the last cached image for that `id`) or
  `quasidock grants --take-back <abs-path>`. Passing `--nofilesystem` omits a
  grant for one run without revoking standing AppContainer access already
  granted to that profile.
- **Grant settlement and orphan cleanup are incomplete in 0.8.0.**
  In `internal/grants/grants.go`, `grants.Settle` currently only removes
  `GrantLoopback` entries and missing paths from `grants\<id>.txt` without
  calling `appcontainer.Unexempt` or `written.Settle`; `SettleOrphans` and
  `NoteLeftovers` are stubs returning `0`/`nil` (so `quasidock repair` settles
  leftover HKCU entries and `grants.RepairMounts`, while orphan host grants
  from deleted stores are not cleaned up). Each store uses its own
  `grants.lock`, so concurrent stores modifying labels on the same host path
  are not serialized across stores for `grants\<id>.txt` changes (mount grants
  use the user-wide `Local\quasidock-mount-grants` mutex and
  `HKCU\Software\Quasidock\Grants\Mounts`).
- **Programs written at low integrity.** Windows launches any executable
  carrying a Low Integrity label at low integrity. `internal/written` watches
  writable container and mount directories with IOCP `ReadDirectoryChangesW`
  (falling back to a 50,000-entry directory walk on buffer overflow) and
  records `.exe`, `.com`, and `.scr` files in `grants\<id>.programs`;
  subsequent non-`soft` runs call `written.Relabel` to enforce `IntegrityLow`.
  Other file types, files written after a run's command exits, or files written
  while a watch is not active keep whatever label they received. Toolchains
  (`zig`, `rust`, `go`, `cmake`, `make`, `meson`, `node`, `python`, `java-sdk`,
  `maven`, `gradle`, and the GTK/Qt/PHP SDKs) default to `"sandbox": "soft"` so
  compiled binaries do not inherit a low label.
- **Protected host folders (`LabelRefusal` / `grants.Own`).** Drive roots,
  `USERPROFILE` or ancestors containing it, Quasidock's store root,
  `quasidock.exe`'s directory, and ancestors or descendants of either cannot be
  granted write access: `cwd` grants in those folders downgrade to read-only
  under `low` (for example `npm init` in `%USERPROFILE%`) and refuse write
  access under `appcontainer`.
- **Deployed files cannot be deleted directly.** Files under `deploy\<hex>\`
  carry a `DENY` `FILE_MODIFY` ACL entry for the current user (`acl.Protect`),
  blocking manual deletion in Explorer or `rm -rf`. Remove images with
  `quasidock rmi` first, or reset ACLs (`icacls <store> /reset /T`) and run
  `quasidock repair`.
- **Drive ACLs without ownership/DACL rights.** `--sandbox=low` needs
  permission to label files and temporarily adjust permissions if a secondary
  drive grants users only Modify; writable grants fail on paths the user
  neither owns nor can modify permissions on.
- **`--sandbox=low` confines writes only.** Read access and network access
  remain open under `low`. System32's `ping` fails at low integrity (`transmit
  failed. General failure.`) because Windows blocks ICMP there.
- **Registry isolation covers only declared keys.** Container `HKEY_CURRENT_USER`
  redirection (`RegLoadAppKey`) isolates only the subkeys listed in the
  image's `registry` field; no CLI flag adds extra keys. Under `low` or
  `appcontainer`, writes to other `HKCU` keys fail; under `soft`, they modify
  the host registry and remain after `quasidock rm`. Because declared keys
  occupy real `HKCU` while running, running an uncontainerized copy of the app
  or two containers sharing the same `registry` key at the same time
  conflicts. Inside an AppContainer, creating multiple missing subkey levels in
  a single call from the `HKCU` root fails unless opened relative to the
  granted key handle.
- **Mount restrictions.** `-v` / `--mount` targets must be `${data}` or
  `${data}/<subpath>`, read-only (`:ro`) mounts and overlapping targets are
  rejected as unsupported, and reparse points anywhere in a bind source or its
  ancestors are refused. Quasidock does not create automatic junctions for
  user shell folders (`Desktop`, `Documents`, `Downloads`), so apps whose
  Save As dialogs open `Documents` without an explicit `-v` mount see the
  container's `data\home` layout instead.
- **Services and port forwarding.** Services bind to loopback only (exposing
  services off-host requires administrator firewall rules). `-p` / `--publish`
  flags are not supported yet (use image `services` and `--port=<svc>=<port>`).
  Under `appcontainer`, containers without loopback exemptions reach only their
  declared `uses` ports forwarded one connection at a time through
  `quasidock-relay` (with a 2 s connect timeout), and allocated internal ports
  in `ports\<id>.txt` stay fixed until `quasidock rmi`. Loopback exemptions
  are a machine-wide Windows setting that can be blocked by domain policy and
  are enabled for `--share=network` containers unless
  `QUASIDOCK_LOOPBACK_EXEMPTION=0`.
- **`-t` / `--tty` and ConPTY Ctrl+C testing.** `quasidock run` accepts `-t`
  / `--tty` (`Config.Tty`), but does not allocate a ConPTY pseudo-console; runs
  use pipes and attach `\\.\NUL` to stdin unless `-i` is given. Background `-i`
  keeps a supervisor-owned pipe open for later attachment. Attach has no terminal
  allocation or resize handling; it supports detach keys for interactive `-t`
  sessions with console stdin and preserves redirected bytes. Its Ctrl+C forwards
  the existing Ctrl+Break-based TERM request, which applications may ignore.
  `tests/ctrlc.sh` builds `node` and runs `tests/cmd/ctrlc/main.go`, whose
  `CreatePseudoConsole` helpers are implemented while `main()` currently exits
  `0` without running a live interactive Ctrl+C test
  ([iteration 2](iteration-2.md)). The offline CLI lifecycle test separately checks
  that a foreground application receives native Ctrl+C through a pseudoconsole.
  Attachment tests also check TERM forwarding, keyboard detachment, and console
  input mode restoration through a pseudoconsole.
- **Exec terminal and inspection support.** `exec` and `container exec` support
  running containers only, with `-d`, `-i`, `-t`, `-e`, and `-w`. Exec `-t` is
  pipe-backed like `run -t`: no ConPTY, resize handling, or detach keys. Exec
  inherits startup environment/cwd, not changes the primary process makes later.
  Foreground disconnect or Ctrl+C ends that exec tree; normal exec completion
  also ends remaining descendants. Detached output is discarded, exec output
  never enters container logs, and there are no persistent exec records or exec
  inspection API. Older live owners must be stopped and started with a binary
  that supports exec.
- **Windows Server 2025 AppContainer restrictions.** Server 2025 blocks opening
  `NUL` inside AppContainers and blocks path lookups on `zig env`,
  `kpathsea` (`GetLongPathNameW`), CPython `getpath`. BusyBox `sh -c`
  under `appcontainer` can exit before the
  last command unless a trailing statement follows (`sh -c "cc x.c; true"`).
  DirectWrite in AppContainers also cannot load per-user fonts, and Windows
  refuses taskbar progress/button updates under `low` and `appcontainer`.
- **Standard-user, multi-user, and profile edge cases.** Multi-user sharing of
  one machine, domain-restricted accounts, and apps that resolve folders with
  `SHGetKnownFolderPath` instead of environment variables are largely untested
  (OpenJDK works around `SHGetKnownFolderPath` via `-Duser.home=${data}\home`).
  MSVC builds run under never-signed-in accounts can leave Windows WebCache
  processes holding build temp directories until a later `quasidock prune`.
- **Base image composition limit.** `from` is single-level only; base images
  that themselves declare `from` are rejected, and all base aliases must resolve
  to distinct image IDs.

## Registry, signing, and OCI

- **Catalog and search.** `quasidock search` queries only the configured
  registry's `catalog` image (`ghcr.io/quasidock/quasidock` or `QUASIDOCK_REGISTRY`);
  there are no multiple remotes, icons, or screenshots. Seven published tool and
  SDK recipes currently omit `name` and `summary`: `busybox.json`, `zig.json`,
  `rust.json`, `cmake.json`, `meson.json`, `cargo-c.json`, and `gettext.json`.
- **TLS certificate trust ([certificate trust](networking.md#certificate-trust)).**
  BusyBox `wget` performs no certificate verification. When Java, Python, or
  Quasidock (`${roots}`) reads Windows' root store, Windows does not trigger
  automatic root downloads for uncached roots. Schannel `curl` (CMake, Cargo)
  enforces strict revocation checks (`revocation status is unknown` behind
  inspecting proxies or CAs without CRL/OCSP), whereas MinGit's git on Schannel
  permits them. Changes to Windows' root store do not reach running containers'
  `${roots}` files, and `quasidock prune` removes unheld `certs\roots-*.pem`
  bundles. MinGit ignores host `C:\Program Files\Git\etc\gitconfig`.
- **Docker credentials.** Quasidock uses username/password/token entries and
  `.exe` credential helpers on `PATH` (not batch wrappers); OAuth2 identity-token
  refresh flows are not implemented.
- **Image and binary verification.**
  - `scripts/install.ps1` checks release archives against `SHA256SUMS`;
    `SHA256SUMS.sig` can be verified separately with `cosign verify-blob`
    ([publishing.md](publishing.md), [README.md](../README.md#build-and-try-it)).
    Releases are `x86_64` Windows only and lack Authenticode signatures, so
    Microsoft Defender ML models can still flag specific binaries by exact bytes
    ([findings](findings.md#other-findings),
    [findings](findings.md#defender-and-exact-bytes)).
  - `internal/trust/quasidock.pub` verifies `ghcr.io/quasidock/quasidock` only; other
    registries require `quasidock trust add`. During `quasidock pull`, wrong
    signatures abort while missing signatures warn and continue (`quasidock
    signature check` requires a valid signature). Keys are ECDSA P-256 classic
    `simplesigning` payloads (`--new-bundle-format=false` or
    `quasidock signature`), with no TUF freshness, key revocation list, or Rekor
    transparency enforcement, and the `catalog` image itself is not verified by
    `search`.
- **Multi-platform indexes and chunked uploads.** `pull` takes the first
  `windows/amd64` manifest in an OCI index (ignoring `os.version` and refusing
  nested index entries), and builds only target `windows/amd64`. `push` mounts
  layers only within the default layout or origin repository. Interrupted
  chunked uploads restart from scratch on `zot`, and blobs larger than 64 MiB
  upload in chunks unless `QUASIDOCK_UPLOAD_CHUNK_SIZE=0` forces a single request.
- **Upstream & OSV report coverage (`scripts/check-upstream.sh`).** Checks
  only top-level recipe URLs, missing transitive dependencies in `Cargo.lock`,
  `go.sum`, `composer.lock`, vendor stages, and bundled libraries (`zlib` inside
  PHP, GDAL, Qt, Node, MinGit, Pillow; see
  [findings](findings.md#zlib-inside-images)). Commits, TeX Live, Ghostscript
  tags, and PHP library/SQLite latest-only pages have version-history limits,
  and OSV records are not filtered to Windows build configurations (see
  [findings](findings.md#advisories)).

## Builds and toolchains

- **Determinism and deflate pinning.** Layer digests depend on Go's
  `compress/flate` level-6 deflate output (`internal/format/layer.go`, pinned
  by `TestGzipDeterministicAndGunzip` in `internal/format/layer_test.go`).
- **Build isolation and concurrency.** Offline `build` steps rely on
  `HTTP_PROXY`/`HTTPS_PROXY=http://127.0.0.1:9` convention rather than OS
  network isolation, while `vendor` stages run with network access and only pin
  the resulting vendored tree. Builds require `B:` to be unused and run one at a
  time. MSVC builds (`"host": ["msvc"]`) depend on the host's Visual Studio
  x64 installation, skip build-cache reuse, and are not reproducible or run in
  CI.
- **BusyBox `make` and `install`.** BusyBox `pmake` ignores `-j` (compiling one
  target at a time unless a recipe parallelizes manually or uses
  `recipes/make.json`) and can corrupt empty command-line macros such as `FOO=`
  ([findings](findings.md#an-empty-macro-in-busyboxs-make)). BusyBox lacks an
  `install` binary, so autotools installs use shell fallbacks unless supplied.
- **Project directory builds.** Incremental accuracy depends on each tool's
  build cache; moving a checkout resets its `cache\work\<16hex>` key; `.git add
  -f` files ignored by `.gitignore` are still excluded; unskipped symlinks or
  junctions fail the build; `push --sources` never uploads local directory
  sources; and every `quasidock build` hashes all included project files.
- **Spaces in store path and Windows `MAX_PATH`.** Unquoted
  `$(pkg-config ...)` expansions split paths at spaces if `QUASIDOCK_HOME` or
  the username contains spaces, while overly long store paths push deep
  deployments (`rust`'s linker invocation, `vscodium`'s tree) toward Windows'
  260-character limit.
- **Zig (`recipes/zig.json`).** `zig fetch` / `zig build` fail at `low`
  integrity (`CertificateBundleLoadFailure`) and need `--share=network` under
  `appcontainer` (default image uses `soft`). `zig rc` picks up host Visual
  Studio headers if installed unless `/:auto-includes gnu` is passed (GTK's SDK
  passes it), and Zig-linked DLLs without `dllexport` export CRT symbols unless
  excluded.
- **Rust and Go.** Rust builds require `zig` for assembly/C on
  `x86_64-pc-windows-gnu`, compiling crates' C code with Zig's UCRT headers
  against Rust's `msvcrt.dll` libs. Go's standalone runtime image has no C
  compiler (`cgo` builds need `zig` in `sdk` with `-ldflags "-buildid="`), and
  `go mod vendor` / `download` pulls every module in `go.mod` (`zot` vendors ~5
  GB transiently for ~68 MB kept).
- **CMake, Meson, and archive sources.** Standalone `cmake` has no C compiler,
  Ninja 1.13.2 fails with `ReadFile: The handle is invalid.` when subprocesses
  cannot start, and CMake records the build host's `CMAKE_SYSTEM_VERSION`
  unless overridden (`-DCMAKE_SYSTEM_VERSION=10.0.26300`). Several older
  recipes (`gaphor`, `gnome-text-editor`, `gnumeric`, `gtk4-msvc`,
  `gtk4-sdk-base`, `gtk4-sdk`, `postgresql-sdk`, `zstd`) still pin an earlier
  `cmake:4.4.4@sha256:fdc6a6bb...` digest rather than `fa7efc2e` (which strips
  host `Program Files` and registry lookups in `WindowsPaths.cmake`). Zip
  sources store symlinks as plain link-target files, while tar links pointing
  outside the archive or to directories fail the build.

## Language runtimes and GUI stacks

- **TeX Live.** Fails inside AppContainers on Windows builds affected by
  `self-long-name` (`kpathsea` `GetLongPathNameW` error); `tlmgr` cannot mutate
  the read-only deployment (updates rebuild pins with
  `scripts/texlive-pins.sh`); format files are built into `${data}` on first run
  (or every run with `--rm`).
- **GTK, PyGObject, and desktop apps.**
  [`recipes/gtk4-msvc.json`](../recipes/gtk4-msvc.json) is an unpublished MSVC
  experiment (missing translations, SVG loader, HTTP, spellcheck, and CI).
  Downstream GTK 4 images must set their own `share` in `XDG_DATA_DIRS` and
  re-pin base images to receive updates; `gtk4-sdk`'s `libxml2` CMake files
  retain `B:/prefix`; headers embedding `__FILE__` encode deployment hashes
  unless rewritten (`-fmacro-prefix-map`). `gnome-text-editor` has no Yelp help
  and includes only WinSpell + `en_US` Hunspell; Pango does not fall back across
  missing script glyphs in Segoe UI. `pygobject` skips its `sitecustomize.py`
  AppContainer workarounds if invoked with `python -S`, and there is no GJS or
  Vala. `gaphor` requires prebuilt Windows CPython 3.14 wheels for C
  dependencies, `meld` requires `--share=network` under `appcontainer` to push
  over git, and `gnumeric` omits help, Python/Perl/`libgda`/Psiconv/Paradox/
  Applix plugins and stores preferences in a key file rather than the registry.
- **Qt 6.** Ships Widgets + QML/Qt Quick 6.12 only (no Multimedia, WebEngine,
  Charts, Imagine style, `qmlls`, or Assistant) in a single shared runtime.
  Multi-module QML builds can hit `qmlcachegen` ordering issues (worked around
  by a second pass in `qt6-sdk`). `flameshot` strips `flameshot-cli.exe` and
  its startup option fails under `low` or writes a stale deploy path under
  `soft`. Both GTK and Qt families run CI weekly (`reproduce.yml` on Mondays)
  in addition to push/PR runs.
- **Java.** `JAVA_TOOL_OPTIONS` emits `Picked up JAVA_TOOL_OPTIONS: ...` on
  stderr and is replaced wholesale if user env overrides it; `Path.toRealPath()`,
  `jps`, and `jcmd` fail in AppContainers; `mvn` ignores `.mvn/jvm.config` and
  `MAVEN_OPTS`; Maven/Gradle vendor steps cannot pin version ranges, `latest`,
  or `SNAPSHOT`, and Gradle offline redirection supports Maven layouts only
  (not Ivy); Temurin images omit `jlink`; `scenebuilder` uses `java-sdk` because
  Temurin's JRE drops `jdk.unsupported.desktop`; `logisim-evolution` cannot run
  host FPGA vendor tools without explicit `--filesystem`; and `registry` parent
  keys above a Java app's `Prefs` key can be listed inside an AppContainer.
- **PHP, Composer, and Kanboard.** Upstream MSVC PHP requires Microsoft's
  Visual C++ Redistributable installed on the host. `php-ext` is untested with
  C++ or ZTS PHP and depends on MinGW compatibility headers (`zig-php.h`). At
  `low` integrity, PHP's `is_writable()` returns `false` even on writable paths
  ([findings](findings.md#phps-is_writable-at-low-integrity)), so
  `kanboard` defaults to `soft` (`appcontainer` also works), runs on PHP's
  single-threaded built-in server (plugins installed manually under
  `${data}\plugins`, `.htaccess` ignored), and `composer self-update` is
  blocked by the read-only deployment.
- **PostgreSQL, GDAL, and osm2pgsql.** `postgresql` runs in the foreground
  until its container stops (use `-d` and `quasidock stop`); under
  `appcontainer`, `archive_command`, `restore_command`, `COPY ... PROGRAM`, and
  `psql`'s `\!` cannot invoke store tools through `cmd.exe`, and manual `initdb`
  uses minimal memory defaults instead of `pg-server.exe`'s sizing; ICU
  collations track host `icu.dll` updates. `gdal` omits curl/network drivers,
  external formats (`netCDF`, `HDF`, `PDF`, `WebP`, `JPEG-XL`, `PostgreSQL`,
  `SpatiaLite`), and Python scripts (`gdal_calc.py`, `gdal_merge.py`).
  `osm2pgsql` cannot read `postgresql`'s `pgpass.conf` (`-W` or `--env
  PGPASSWORD=...` needed), links Lua statically (no C Lua modules), and omits
  `osm2pgsql-replication` / `osm2pgsql-gen`.
- **OCI tools.** In `appcontainer`, `oras` directory pulls and `skopeo`
  `oci:`/`dir:` copies fail in Go's `filepath.EvalSymlinks` walking drive roots.
  `skopeo` lacks `containers-storage:` on Windows, `cosign` lacks PKCS#11/PIV
  and untested OIDC/Fulcio/Rekor flows, and none share Quasidock's or Docker's
  Credential Manager logins.

## Lazy pulls (`quasidock pull --lazy`)

- **Whole-blob fallback in 0.8.0 helper.** `lazy.Prepare` (`internal/lazy/lazy.go`)
  fetches the eStargz tail and spawns `quasidock lazy-helper`, which registers
  `deploy\<hex>\` as a Cloud Files sync root and creates placeholders
  (`internal/cloudfiles/cloudfiles.go`), but `internal/lazy/helper.go`
  (`RunHelper`) does not yet connect `CfConnectSyncRoot` on-demand read
  callbacks or `Hydrate` for `prefetch` entries: once `pull` releases
  `lazy\<hex>.pull.lock`, `RunHelper` calls `reader.DownloadAll()` into
  `blob.sparse` (`"QDBLOB01"`), unpacks into `tmp\lazy-unpack*`, waits until
  `!DeploymentInUse`, unregisters the sync root, and swaps the plain files into
  `deploy\<hex>\`. Also `quasidock rmi` does not stop a running lazy helper
  directly (`quasidock prune` calls `lazy.Prune`).
- **eStargz, NTFS, job, and `prefetch` constraints.** Only eStargz layers
  `>= 32 MiB` with chunk digests qualify (smaller layers and gzip runtime
  layers such as `cmake`, `perl`, and `ghostscript` pull upfront). Cloud Files
  placeholders require NTFS (non-NTFS drives such as ReFS Dev Drive, FAT, or
  network shares fall back via `ErrUnsupported`), and detached helpers cannot
  survive inside job objects that disallow breakaway. Only `qt6` and `qt6-sdk`
  currently declare `prefetch` lists, and the first launch right after swapping
  placeholders with unpacked files pays cold-cache startup cost.
