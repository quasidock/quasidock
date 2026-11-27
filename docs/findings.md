# Findings

Empirical findings on Windows sandboxing, OS quirks, and toolchains.

## Does the no-admin stack hold up?

### Summary

- **The `soft` sandbox works for every tool tested.** All 26 checks in Quasidock's
  initial matrix behaved as intended. Each container gets a hermetic environment,
  an isolated data directory (`containers/<id>/data`), and working-directory
  access; `mingit`, `npm`, and `zig` builds all keep their state inside container data.
- **The `appcontainer` sandbox only suits self-contained tools.** `busybox`,
  `ripgrep`, and `python` mostly work and permissions are enforced. Git, Node
  scripts, npm, and Zig failed on the original test build because of two Windows
  restrictions on AppContainers that cannot be lifted without administrator
  rights.
- One reproducibility hole in `soft` mode was found and fixed early: apps could
  modify deployed application files. Deployed layers are made read-only.
- **Re-checked on Windows 11 26300.9550** (see [below](#re-checked-on-windows-11-263009550)):
  on Windows 11 26300.9550, restriction 1 is gone, and `mingit` and `node`
  work under `appcontainer` in
  part. (In Quasidock's current recipes, `node`, `python`, and `zig` set
  `"sandbox": "soft"`, so the matrix passes `--sandbox=appcontainer` and
  `--sandbox=low` explicitly.)
- **Prebuilt GUI apps** keep
  their settings in container data in all three sandboxes, since Windows derives
  AppData from `USERPROFILE`; the registry, `LocalLow`, and real profile folders
  do not follow (see [below](#prebuilt-gui-apps)).
- **The registry**: a key in
  `HKCU` labelled low, or granted to an app's capability SID, can be written by
  the app at low integrity and in an AppContainer. Quasidock keeps an app's
  declared `registry` keys per container in `containers/<id>/data/registry` and
  loads them into `HKCU` during runs via
  [`internal/hkcu/hkcu.go`](../internal/hkcu/hkcu.go) (see
  [below](#the-registry)).

### Matrix

Measured in the first Quasidock version (`ok` = exits 0, `fails` = exits non-zero;
`✗` marks results that differ from the intended behaviour):

| Tool | Check | soft | appcontainer |
|---|---|---|---|
| busybox | runs, exit codes, writes to data dir | ok | ok |
| busybox | app files are read-only | fails (intended) | fails (intended) |
| busybox | write to an ungranted host dir | ok (not enforced) | fails (intended) |
| busybox | background children end with the run | ok | ok |
| ripgrep | runs, searches the granted cwd | ok | ok |
| ripgrep | read an ungranted host file | ok (not enforced) | fails (intended) |
| mingit | `--version` | ok | ok |
| mingit | `config --global`, `init`/`commit`, `ls-remote` over HTTPS | ok | ✗ fails |
| node | `-e`, `fetch` over HTTPS | ok | ok |
| node | script file, `fs.realpathSync`, `npm install` | ok | ✗ fails |
| python | script file, HTTPS with system certificates | ok | ok |
| python | `os.path.realpath` | ok | ✗ fails (returns unresolved path) |
| python | network denied by `--unshare=network` | ok (not enforced) | fails (intended) |
| python | write into its own app dir | fails (intended) | fails (intended) |
| zig | `version` | ok | ok |
| zig | `env`, `init` + `build run` | ok | ✗ fails |

### Why the AppContainer failures happen

**1. Converting a path to a drive letter is denied.**
`GetFinalPathNameByHandleW` with `VOLUME_NAME_DOS` failed with
`ERROR_ACCESS_DENIED`, while the same call with `VOLUME_NAME_NT`
(`\Device\HarddiskVolume3\...`) succeeded. The failing step is mapping the
volume device to `C:`, which goes through the Mount Manager, and AppContainers
could not open it. Git calls this to get its working directory, Python and Node
use it for `realpath`, and Zig uses the same mapping to find its own
executable. Rust's `std::fs::canonicalize` calls the same API, so Rust tools
that canonicalize paths fail the same way when the Mount Manager is denied.

**2. `C:\` itself grants nothing to AppContainers.** The drive root has no
entry for `ALL APPLICATION PACKAGES`, so `lstat("C:\\")` fails with `EPERM`.
Node's JavaScript `realpath` checks every parent folder of a path, so loading
any script file fails, and npm with it.

Neither can be fixed without admin. The root folder's permissions belong to
SYSTEM/TrustedInstaller, and the Mount Manager's access rules are set by
Windows. A probe program calling `GetFinalPathNameByHandleW` with each flag
combination inside the AppContainer ruled out missing grants on parent folders
as the cause.

**3. On Windows Server 2025, `NUL` is denied too** (as seen on GitHub's `windows-2025`
runners). Git opens `/dev/null` as it starts, and fails (`could not open
'/dev/null' for reading and writing: Permission denied`), so even
`git --version` fails there while starting on Windows 11. Go's toolchain opens
`NUL` during builds (`error obtaining buildID for go tool compile: open NUL:
Access is denied`), so Go builds that work under an AppContainer on Windows 11
fail on Server 2025. [`tests/matrix.sh`](../tests/matrix.sh) marks these
AppContainer failures as known gaps, so CI only fails when a result changes.

## Re-checked on Windows 11 26300.9550

After 2026-10-05 updates: several previously
failing AppContainer gaps began passing on the Windows 11 test machine (`git
--version`, `zig env`, Zig builds, and Python's `realpath`) after three Windows
updates (KB5124010, KB5121794, KB5124009) installed on 2026-10-03.

[`tests/cmd/acprobe/main.go`](../tests/cmd/acprobe/main.go) (built with
`go run ./scripts/build.go acprobe` to `out/test/quasidock-acprobe.exe`) checks
these causes from inside an AppContainer:

| Check | soft | appcontainer |
|---|---|---|
| `final-path-dos`: the working directory's path with its drive letter | ok | ok |
| `self-path-dos`: the same for its own executable | ok | ok |
| `final-path-nt`: the NT path, as a control | ok | ok |
| `mount-manager`: opening the Mount Manager | ok | ok |
| `drive-root`: the drive root's attributes | ok | denied |
| `nul`: NUL, to read and write | ok | ok |

- **Restriction 1 is gone on Windows 11 26300.9550.** AppContainers can open the
  Mount Manager there, so paths resolve to drive letters; Python's `realpath`,
  Zig, and CMake work.
- **Restriction 2 remains, and is wider than the drive root.** An AppContainer
  cannot read the attributes of any directory that does not grant it access,
  including the drive root, `C:\Users`, and parents of paths Quasidock grants.

What the remaining AppContainer failures need:

- **`mingit`** fails in repository discovery: it stats the working directory's
  parent to stop at a filesystem boundary and dies when that is denied (`failed
  to stat '...Temp'`). With `GIT_DISCOVERY_ACROSS_FILESYSTEM=1`, which
  [`recipes/mingit.json`](../recipes/mingit.json) sets, Git skips that stat so
  `config --global` and `ls-remote` work. When creating files, Git still stats
  each directory of their path from the drive root, so `init` and `commit` fail.
- **`node`** resolves script and module paths with its JavaScript `realpath`,
  which calls `lstat()` on every directory from the drive root down. With
  `NODE_OPTIONS=--preserve-symlinks --preserve-symlinks-main`, which
  [`recipes/node.json`](../recipes/node.json) sets, it skips that resolution so
  scripts, `npm install`, and npm's global commands work. `fs.realpathSync` and
  `npm install -g` still fail because npm itself calls `lstat()` on
  `C:\Users`, while `fs.realpathSync.native` works.

[`tests/matrix.sh`](../tests/matrix.sh) runs
[`tests/acprobe.sh`](../tests/acprobe.sh) first and expects each known gap
whose causes are lifted to pass, so regressions fail the matrix while unresolved
causes remain marked as known gaps.

### A low-integrity sandbox

Tested first on 2026-10-05 by running `acprobe --low <command>` with a
copy of the user's token labelled low integrity (requiring no privileges), with
deployments ungranted to AppContainers and `home` / working directories labelled
low (`icacls <dir> /setintegritylevel low`):

| | low integrity |
|---|---|
| The probe's six checks | all ok |
| `mingit`: `--version`, `config --global`, `init` | ok |
| `node`: a script, `npm install`, `fetch` over HTTPS | ok |
| `python`: a script, `realpath` | ok |
| `zig`: `env`, `cc` | ok |
| Writing to a directory not labelled low, or into the store | denied |
| Reading a file not labelled low | ok |

- **Every tool tested works**, unlike under AppContainer: low integrity restricts
  writing, not reading or path resolution.
- **It confines writes only.** Reading and the network stay open, as in `soft`.

## The low sandbox

Measured 2026-10-05 on Windows 11 26300.9550.

### Which grants keep low integrity out

Python's deployment was copied without prior grants, given one inherited
read-and-execute allow ACE at a time, and launched at low integrity with
`acprobe --low`:

| The entry's SID | At low integrity |
|---|---|
| none (the control) | runs |
| an AppContainer's package SID, `S-1-15-2-` and seven numbers | fails: every open is denied |
| `ALL APPLICATION PACKAGES` (`S-1-15-2-1`) | runs |
| `ALL RESTRICTED APPLICATION PACKAGES` (`S-1-15-2-2`) | runs |
| a capability SID (`S-1-15-3-1024-` and eight numbers) | runs |

- **Only package SIDs block low integrity**, denying file opens, directory
  listings, and path resolution (`final-path-dos`, `self-path-dos`, and
  `final-path-nt` all fail with `ACCESS_DENIED`) despite the user's own full
  access ACE.
- **Executables still start**, because the parent process maps the executable,
  which is why `busybox` (having no external DLLs) appeared unaffected while
  Python failed loading DLLs and Zig failed resolving its own executable path.
- **Capability SIDs isolate AppContainers without breaking low integrity.**
  Instead of granting a package SID, Quasidock derives a per-app capability SID
  from `"quasidock." + id` via `DeriveCapabilitySidsFromName` in
  [`internal/appcontainer/appcontainer.go`](../internal/appcontainer/appcontainer.go).
  Only that app's AppContainer runs hold this capability, keeping cross-app
  AppContainer isolation intact while low integrity ignores capability ACEs.
  During setup, [`internal/run/run.go`](../internal/run/run.go) and
  [`internal/acl/acl.go`](../internal/acl/acl.go) revoke any legacy package SIDs
  from store directories (`RevokePackageSids`) and the profile SID from host
  paths.

### Programs written at low integrity

Investigated on Windows 11 26300, unelevated, by having `busybox` copy a probe program
([`tests/cmd/hostprobe/main.go`](../tests/cmd/hostprobe/main.go), which prints
its integrity level) in each sandbox and checking labels with `icacls`:

| The program | Its label | Starts at |
|---|---|---|
| written at low into an `(OI)(CI)` low folder | `Low:(I)(NW)`, inherited | low |
| written in an AppContainer into a granted, unlabelled folder | `Low:(NW)`, its own | low |
| either, given an own medium label (`AddMandatoryAce`, no flags) | `Medium:(NW)` | medium |
| the same, after its folder is labelled `(OI)(CI)` low again | `Medium:(NW)`, kept | medium |

Windows starts a program at the lower of the launching process's integrity level
and the executable file's mandatory label:

- **Stopping label inheritance requires a privilege.** `SetNamedSecurityInfoW`
  with `PROTECTED_SACL_SECURITY_INFORMATION` or
  `UNPROTECTED_SACL_SECURITY_INFORMATION` fails with error 1314
  (`ERROR_PRIVILEGE_NOT_HELD`, `SeSecurityPrivilege`), even for the file's
  owner. Setting an explicit medium label requires only `WRITE_OWNER`, and an
  explicit medium label overrides an inherited low label.
- **A low-integrity process cannot write into a file with an explicit medium
  label**, though it can delete it or rename another file over it via the
  parent low-labelled folder's `FILE_DELETE_CHILD` permission.
- **`ReadDirectoryChangesW` from medium integrity** sees writes and renames by
  low and AppContainer processes (including subfolders, actions 1, 3, 4, and 5).
  Quasidock uses [`internal/written/written.go`](../internal/written/written.go)
  to watch writable directories and relabel newly written `.exe`, `.com`, and
  `.scr` files back to medium once runs finish.
- **Clearing a label from low integrity makes it permanent.** Writing an empty
  label SACL from a low-integrity process leaves an explicit low label on the
  folder and every descendant (`unlabelData` in
  [`internal/run/run.go`](../internal/run/run.go) only unlabels unmounted `data`
  on `soft` runs, and [`internal/acl/acl.go`](../internal/acl/acl.go) skips
  unlabelling with a warning when `BelowMedium()` is true).
- **CI's Kanboard failure**:
  [`tests/published.sh`](../tests/published.sh) previously ran its smoke checks
  inside a scratch working directory holding the pull store. When low-integrity
  `mingit` labelled `cwd`, the inherited low label reached deployed layers, and
  `php.exe` started at low integrity even inside Kanboard's `soft` run — hitting
  PHP's `is_writable()` check ([below](#phps-is_writable-at-low-integrity)).
  `tests/published.sh` now runs from a separate `$work\cwd`, and
  [`internal/run/run.go`](../internal/run/run.go) warns on `soft` runs if the
  target executable has a low integrity label.

### The matrix

In Zigsaw iteration 9's 12-tool run of [`tests/matrix.sh`](../tests/matrix.sh),
running each check in `soft`, `appcontainer`, and then `low` produced 0
mismatches:

| Tool | Check | soft | appcontainer | low |
|---|---|---|---|---|
| busybox | write to an ungranted host dir | ok (not enforced) | fails (intended) | fails (intended) |
| ripgrep, bat, zstd | read an ungranted host file | ok (not enforced) | fails (intended) | ok (not enforced) |
| mingit | `init`/`commit` in cwd | ok | fails (known gap) | ok |
| node | `fs.realpathSync`, `npm install -g` | ok | fails (known gap) | ok |
| python | network denied by `--unshare` | ok (not enforced) | fails (intended) | ok (not enforced) |
| all twelve tools | every other check | as in soft | | as in soft |

- **Labels propagate.** Labelling a directory `(OI)(CI)(NW)` low applies to all
  existing and newly created descendants, and removing the label removes theirs.
  In Zigsaw iteration 9's timing, labelling 20,000 files took `2.2 s`, matching
  the `2.4 s` needed to grant them to an AppContainer.
- **The first low-integrity labelling of a populated data directory** was the
  one-time slow step in Zigsaw (`zig`'s first low run with its cache took
  `9 s`). In Quasidock, each container starts with an isolated
  `containers/<id>/data` directory unless attached to existing state via `-v` or
  `--mount` ([iteration 6](iteration-6.md), [iteration 7](iteration-7.md)).
- **Windows Server 2025**: CI runs [`tests/matrix.sh`](../tests/matrix.sh) under
  administrator runners, unlike the unprivileged Windows 11 development tests.

## The user's folders

Measured 2026-10-14, Windows 11 26300.
Before Quasidock temporarily exposed named user folders (`Documents`, `Downloads`,
etc.) via NTFS directory junctions inside an app's home, probes verified that
neither recursive deletes nor ACL changes traversed through a junction into its
target, and that sandboxes enforced target permissions across junctions.

*(Feature update: Quasidock removed named user-folder permissions like
`--filesystem=documents` and `--file` in 0.3 ([iteration 3](iteration-3.md)).
The same NTFS junction findings now underpin Quasidock 0.7's `-v`/`--mount`
named volumes and bind mounts in [`internal/mounts/mounts.go`](../internal/mounts/mounts.go)
and [`internal/run/mounts.go`](../internal/run/mounts.go), tested by
[`tests/mounts.sh`](../tests/mounts.sh) and
[`tests/sandbox.sh`](../tests/sandbox.sh).)*

| Probe (measured 2026-10-14) | Result |
|---|---|
| Zig 0.16's `Io.Dir.deleteTree` on a tree containing a junction | removes the junction; the target's files stay |
| Zig 0.16's `Dir.walk` | reports the junction as a `sym_link` and does not enter it |
| Git Bash's `rm -rf` | removes the junction only |
| Inherited low integrity label and AppContainer grant (`SetNamedSecurityInfoW`) on the parent folder | changes the junction's own ACL, not the target folder's |
| Labelling and granting a parent containing a junction whose target denies Everyone listing (such as `Documents\My Music`) | succeeds; the target stays untouched and grants restore cleanly |
| Low-integrity process traversing a junction | reads; writes only once the target is labelled low |
| AppContainer process traversing a junction | neither reads nor writes until the target is granted to its capability |
| `SHGetKnownFolderPath` for Documents when `home\Documents` is a junction | returns `home\Documents`, writable across all three sandboxes |

In Go, [`internal/mounts/mounts.go`](../internal/mounts/mounts.go)'s
`RemoveTree`, `os.ReadDir`, and `filepath.WalkDir` likewise treat NTFS
junctions as reparse points without descending into targets, and
[`internal/run/run.go`](../internal/run/run.go) labels/grants unmounted
`containers/<id>/data` before junctions exist (or manages mount sources through
dedicated mount ownership records).

- `User Shell Folders` on Windows 11 maps the standard user folders relative to
  `%USERPROFILE%\<name>` (`Personal` for Documents, `My Video` for Videos, and
  Downloads by GUID), which is why redirecting `USERPROFILE` redirects them.
- Creating an NTFS directory junction requires no special privilege, unlike a
  Windows symbolic link (which requires Developer Mode or administrator rights).
- **Open dialogs and Quick access**: in Zigsaw iteration 18, Notepad++'s Open
  dialog accepted `shell:Personal` across all three sandboxes once `Documents`
  was present, and pinned folders in the navigation pane only appeared once
  `%APPDATA%\Microsoft\Windows\Recent\AutomaticDestinations` existed in the
  container profile (Desktop is never pinned by default). Quasidock still
  pre-creates `AutomaticDestinations` inside every container profile in
  [`internal/run/run.go`](../internal/run/run.go).

## The registry

[Zigsaw iteration 19](../legacy/iteration-19.md), 2026-10-15, Windows 11 26300.
[`tests/cmd/regprobe/main.go`](../tests/cmd/regprobe/main.go) (built with
`go run ./scripts/build.go regprobe`) and
[`tests/regprobe/app.json`](../tests/regprobe/app.json) verify how unprivileged
per-app registry persistence works without administrator rights.

At medium integrity (as Quasidock itself):

| Probe | Result |
|---|---|
| `RegLoadAppKey` on a file in a user-owned directory | loads private hive creating `user.dat`, `.LOG1`, and `.LOG2`, accessible only via returned handle |
| Loading the same hive file again while loaded | shares the same loaded hive across handles |
| `RegCopyTree` from an `HKCU` key into the app hive | fails with `ERROR_ACCESS_DENIED`, even for values alone |
| `RegCopyTree` from the app hive into `HKCU` | works |
| Value-by-value copy (`RegEnumValue`, `RegSetValueEx`, recursive subkeys) both ways | preserves all value types (`SZ`, `EXPAND_SZ`, `MULTI_SZ`, `DWORD`, `QWORD`, `BINARY`), default values, and subkeys; 2,000 values copied in 109 ms |
| `RegRenameKey` in `HKCU` | works |

Inside sandboxes on prepared vs unprepared `HKCU` keys:

| App operation during run | soft | low (key labelled low, inheritable) | appcontainer (key granted capability SID, inheritable) |
|---|---|---|---|
| reads, sets, and deletes values | ok | ok | ok |
| creates one subkey (`RegCreateKeyEx` from `HKCU` with full path) | ok | ok | ok |
| creates two missing levels at once from `HKCU` | ok | ok | `ERROR_ACCESS_DENIED` |
| creates two missing levels at once from an open handle to the prepared key | ok | ok | ok |
| writes a sibling key Quasidock did not prepare | ok | denied | denied |
| reads a key Quasidock did not prepare | ok | ok | denied |

- [`internal/hkcu/hkcu.go`](../internal/hkcu/hkcu.go) and
  [`internal/regkey/regkey.go`](../internal/regkey/regkey.go) copy declared keys
  value by value between `containers/<id>/data/registry/user.dat` and `HKCU`,
  labelling them low or granting the app's capability SID (plus `KEY_READ` on
  ancestor keys; see [Java](#java)). With `--rm`, the hive is discarded unless
  backed by a `-v`/`--mount` mount.

## Projects with their own recipe

When invoked without arguments or with a directory, `quasidock build` looks for
`quasidock.json` at the project root and stages sources into `B:\src\<stage>`
via [`internal/builder/project.go`](../internal/builder/project.go).

- **Zigsaw's self-build benchmark**: during the Zig era, Zigsaw carried its own
  `zigsaw.json` building three stripped binaries via `zig build -Dstrip` in
  `B:\src\zigsaw` bit-identically across stores and matching
  [`scripts/package.sh`](../scripts/package.sh). In a fresh store it took
  157 s plus 30 s to pull `zig`, dropping to 67 s once Zig's global cache was
  warm and under 1 s when inputs were unchanged. Keeping `.zig-cache` saved
  only 51 s vs 60 s in [Zigsaw iteration 35](../legacy/iteration-35.md) because
  `zig` recompiles the main binary unit whole, and hashing Zigsaw's 52 source
  files took 10–35 ms. (Quasidock itself is written in Go, builds with
  `go run ./scripts/build.go [--strip]`, and has no `quasidock.json`.)
- **`.gitignore` matching**:
  [`internal/builder/gitignore.go`](../internal/builder/gitignore.go) matches
  `git ls-files -co --exclude-standard` on a fixture with nested `.gitignore`
  files, `!` negations, anchored patterns, `**`, case-insensitive matches on
  Windows (`X.LOG` ignored by `*.log`, matching `core.ignorecase`), filenames
  with spaces, and empty directories (`tests/build.sh` `projects` section).
- **Reparse points**: Windows reports both NTFS junctions and symbolic links as
  reparse points (`os.ModeSymlink` in Go), so directory walks in
  [`internal/builder/project.go`](../internal/builder/project.go) refuse or
  skip them without descending into targets.

## Zig's packages, and ZLS

- **Zig's package cache layout**: Zig unpacks project dependencies beside
  `build.zig.zon` into `zig-pkg` (one directory per package hash) and keeps
  tarballs under `p\<hash>.tar.gz` in its global cache (`ZIG_GLOBAL_CACHE_DIR`,
  pointing to `${cache}` in builds). `--system <pkgdir>` only accepts unpacked
  `zig-pkg` directories, failing on `p\`, so a `vendor` step must pin `zig-pkg`
  rather than relying on Zig's warm global cache.
- **ZLS builds reproducibly offline**: measured on Zigsaw with ZLS 0.16.1,
  vendoring its three packages took under 40 s after pulling `zig` and the
  build took 1.5 minutes, reproducing identically across clean stores.
  [`recipes/zls.json`](../recipes/zls.json) now builds ZLS 0.17.0 on Zig
  0.17.0.
- **Single-runtime sharing**: placing `zig` in `from` puts `zig.exe` and its
  standard library on container `PATH` automatically without listing it in `sdk`
  as well (Quasidock forbids sharing an alias across `from` and `sdk` in
  [recipes](recipes.md)). Running a project's `build.zig` took ~10 s with cold
  caches across `soft`, `low`, and `appcontainer` runs in Zigsaw.
- **Interactive stdio**: `quasidock run -i` passes standard input and output
  directly through (`quasidock` removed Zigsaw's shims in
  [iteration 2](iteration-2.md)), with ZLS hover responses arriving in under
  0.1 s.
- **Diagnostics capability**: ZLS only emits `textDocument/publishDiagnostics`
  to LSP clients advertising that capability in `initialize`.
- **Windows Server 2025**: in a CI reproducibility run on Windows Server 2025
  Github runner, `zls` in an AppContainer hit `self-path-dos` (`Mount Manager`
  denied), breaking `zig env` and `build.zig` execution (`hover` returned
  `null`, while parser syntax diagnostics still appeared).
  [`tests/zls.sh`](../tests/zls.sh) handles those checks as known gaps when
  [`tests/acprobe.sh`](../tests/acprobe.sh) reports `self-path-dos` failing.

## GTK 3 and Gnumeric

- **GTK 3 runtime**: GTK 3.24.52 and ATK (`at-spi2-core` 2.60.7 with
  `-Datk_only=true`) build with Meson and `zig` against GTK 4's SDK in ~3
  minutes (18 MB compressed runtime alongside GTK 4's 52 MB).
- **GTK 3 missing-Desktop warning**: when a container has no `Desktop` folder
  and `g_get_user_special_dir()` returns `NULL`, GTK 3's file chooser, places
  sidebar, and file button compare `NULL` against the home path and call
  `g_file_new_for_path(NULL)` with critical warnings; GTK 4 guards against
  `NULL`.
- **`zig cc` linker flags**: `zig cc` ignores `-mwindows` (emitting an unused
  argument warning), requiring `-Wl,--subsystem,windows` for GUI subsystems such
  as Gnumeric. It also ignores `-Wl,--export-all-symbols`: `lld` exports
  everything only when nothing carries `dllexport`, not when some symbols are
  already marked `dllexport` (unlike GNU ld with that option).
- **Autoconf `configure` under BusyBox `sh`**: BusyBox `sh` makes `configure`
  choose `:` as `PATH_SEPARATOR` (splitting `D:\...` at the drive colon) and
  leave `ac_executable_extensions` empty (missing `pkg-config.exe`). Setting
  `PATH_SEPARATOR=;` and `ac_executable_extensions=.exe` fixes discovery
  globally.
- **BusyBox `make` (`pdpmake`) quirks**: expands GNU make's `$(shell ...)` to
  empty (`VAR != command` works with escaped `\#`), lacks pattern rules, and
  sets `$<` outside inference rules to any out-of-date prerequisite such as
  `Makefile` rather than the first prerequisite. `intltool`'s `po/Makefile` uses
  `$(shell ...)`, silently dropping Gnumeric's translations until fixed.
- **BusyBox `sh` shebangs**: handles `#!/usr/bin/perl` via `PATH`, but rejects
  interpreter lines naming a Windows drive letter (`Exec format error`), such
  as scripts generated by `intltool`'s `configure`.
- **Strawberry Perl text mode**: reads files with CRLF translation unless set
  otherwise, corrupting PNG bytes embedded by `goffice`'s script (`GdkPixbuf`
  critical warnings on startup); `PERLIO=perlio` restores Unix binary reads.
- **Strawberry Perl XS DLLs**: `c\bin\*__.dll` (40 DLLs needed by XS modules
  such as `XML::Parser`) were stripped when omitting Strawberry's GCC `c\`
  tree; [`recipes/perl.json`](../recipes/perl.json) keeps them beside
  `perl.exe` (adding 29 MB).
- **Gnumeric 1.12.62 upstream Windows build gaps**: its symbol-export generator
  misses new analysis headers (`ssconvert`), `complex.h`, and array-size
  function parameters (`sheet_style_apply_border` needed by the Excel plugin),
  its Applix plugin references an omitted exporter, and it calls non-existent
  `g_option_context_set_delocalize`. Letting `lld` export all symbols bypasses
  the `.def` export generator.
- **SDK `__FILE__` store path leaks**: `libgtk-3-0.dll` embedded the builder's
  store path to GLib's `gobjectnotifyqueue.c` via `g_return_if_fail` (`__FILE__`)
  because GTK 4's SDK is deployed inside a store path rather than `B:\prefix`.
  [`recipes/gtk3-sdk.json`](../recipes/gtk3-sdk.json) passes
  `-fmacro-prefix-map` to normalize the SDK path across stores.
- **Build time and size**: Gnumeric takes 16–24 minutes to build (mostly
  BusyBox libtool linking 37 plugins serially); its compressed image is 15 MB, of
  which 8.6 MB is translations.

## Introspection, PyGObject and Meld

- **`gobject-introspection` Windows assumptions**: `giscanner/ccompiler.py`
  assumed MSVC or an MSYS2 shell (`sh.exe` is not on build `PATH` without
  adding it, searches standard linker paths rather than SDK `lib`, and passes
  `-lfoo` rather than `libfoo.dll.a`). Patched in
  [`recipes/gtk4-sdk/gobject-introspection.patch`](../recipes/gtk4-sdk/gobject-introspection.patch).
- **`dlltool` vs `nm` for import libraries**: with LLVM `dlltool`, `giscanner`
  uses `nm` to inspect import archives.
- **`subst` drive scanner bug**: `giscanner`'s Python driver resolved paths via
  `os.path.realpath` (following `B:` to the real staging folder) while its C
  lexer used `GetFullPathName` (keeping `B:`), silently dropping every
  declaration from generated headers such as `./gdk-pixbuf/gdk-pixbuf-enum-types.h`
  (`GType` enums and `Gtk.get_major_version`). Switching the lexer to
  `GetFinalPathNameByHandle` fixes it.
- **`winflexbison`**: its sources require MSVC and MSVC headers, so GI uses a
  pinned release binary archive on build `PATH` only during GI compilation.
- **CPython extensions with `zig`**: NuGet's `python` package supplies headers
  and `libs\python314.lib`, and `pyconfig.h` enables `MS_WIN64` under
  `__MINGW32__` (defined by `zig`), letting `pycairo` and PyGObject build via
  Meson with `-Dpython=` pointing to NuGet's `python.exe` (since Meson runs on
  the headerless embeddable Python).
- **DLL loading since Python 3.8**: extension modules and `ctypes` ignore
  `PATH`; `gi` adds directories from `PYGI_DLL_PATH` using
  `os.add_dll_directory()`, while GLib typelibs still load DLLs via Win32
  `LoadLibrary` (`PATH`), so Meld needed GtkSourceView's `bin` added to
  `PATH`.
- **`ctypes.cdll[name]` quirks**: raises `AttributeError` instead of `OSError`
  on missing DLLs and passes Python `str` as `wchar_t *`. Meld caught
  `FileNotFoundError` and passed `str` to `bindtextdomain`, causing libintl to
  read `"D:\..."` as `"D"` and leave Glade UI files untranslated.
- **Multiprocessing pipes in AppContainer**: AppContainers require named pipes
  under `\\.\pipe\LOCAL\`, whereas CPython's `multiprocessing` uses
  `\\.\pipe\pyc-...` (`PermissionError`, breaking Meld diffs).
  [`recipes/pygobject/sitecustomize.py`](../recipes/pygobject/sitecustomize.py)
  rewrites the pipe prefix under `LOCAL\`.
- **Meld font backend**: upstream Meld forces `PANGOCAIRO_BACKEND=fontconfig` on
  Windows even though GTK here ships only the `win32` font map, causing blank
  rendering; [`recipes/meld.json`](../recipes/meld.json) forces `win32`.
- **`gnome.post_install`**: invokes `update-desktop-database`, which fails on
  Windows if enabled in Meson.

## GTK's extension point and Gaphor

*(Feature update: Quasidock removed extension points (`--extension`) and GTK's
extension joining in 0.3 ([iteration 3](iteration-3.md)), and later removed the
standalone `gtk4-hunspell-fr` and `gtk4-hunspell-vi` recipes. The GLib,
`enchant`, Gaphor, Python `tempfile`, `pip`, and Graphviz findings remain
durable.)*

- **GLib `XDG_DATA_DIRS` replacement semantics on Windows**: when unset, GLib
  searches `C:\ProgramData`, `C:\Users\Public\Documents`, the app's `share`,
  GLib's `share`, and the caller DLL's sibling `share`. When set explicitly, it
  searches *only* the listed directories; `enchant`'s relocatable prefix still
  loaded its bundled `en_US` hunspell dictionary beside `enchant`.
- **`enchant` dictionary tags**: LibreOffice's language-only files (`fr.aff` /
  `fr.dic`) satisfy region-specific requests such as `fr_FR`.
- **Gaphor schema lookup order**: Gaphor checks settings non-recursively
  (`lookup(id, False)`), querying only the first schema source
  (`GSETTINGS_SCHEMA_DIR`, user data dir, or the first system data dir). When
  Gaphor's `share` came last in `XDG_DATA_DIRS` in Quasidock's extension tests,
  settings were silently dropped even though Gaphor's recursive self-test
  passed.
- **Python 3.13+ `tempfile` in AppContainer**: `os.mkdir(path, 0o700)` applies a
  protected DACL (`SYSTEM`, `Administrators`, `OWNER RIGHTS`) lacking the
  AppContainer capability SID, so the container creates the directory and then
  fails opening files inside it. `pydot`'s vendored `tempfile` retries 10,000
  times on `PermissionError`, hanging Gaphor's auto-layout (while `low` runs
  succeed via inherited labels).
  [`recipes/pygobject/sitecustomize.py`](../recipes/pygobject/sitecustomize.py)
  strips `0o700` directory protection in AppContainers.
- **`pip` in vendor steps**: PyGObject's SDK Python (NuGet) ships `pip` 26.2.1
  as `python3.exe` (Meson's bundled embeddable Python comes first on `PATH` and
  lacks `pip`). `pip download --only-binary=:all: --platform win_amd64
  --python-version 3.14 --implementation cp --require-hashes` fetched 16 wheels
  deterministically. `pip install --target` writes entry `.exe` launchers
  embedding the build store's Python path and records their hashes in `RECORD`,
  so [`recipes/gaphor.json`](../recipes/gaphor.json) deletes both the generated
  launchers and their `RECORD` entries to keep images reproducible.
- **`gi.events`**: PyGObject 3.58.1's asyncio integration (`GLibEventLoopPolicy`
  and `socketpair`) works in `soft`, `low`, and `appcontainer` on Windows.
- **Graphviz 16.1.0**: builds cleanly with CMake + `zig` as static libraries
  (`BUILD_SHARED_LIBS=OFF`, baking plugins directly into `dot.exe` with no
  plugin config file) in 261 build steps using `winflexbison`. `dot.exe`
  imports only `KERNEL32` and the UCRT (build date becomes `"0"` without Git;
  upstream's release tarball omits CMake files while GitLab's tag tarball
  contains a broken symlink).

## One build of each runtime, and git for Meld

- **Pin publication checks**: in an earlier run,
  a Reproduce run passed locally while an updated PyGObject SDK pin
  (`3498644a`) had never been pushed to `ghcr.io`, masking downstream skipped
  images (`gaphor`, `meld`, `pygobject`).
  [`scripts/check-pins.sh`](../scripts/check-pins.sh) verifies every pinned
  image reference exists on the registry.
- **Embeddable Python AppContainer gaps**: matches PyGObject's Python (`tempfile`
  `PermissionError` and `multiprocessing` pipe `WinError 5`), while `soft` and
  `low` runs succeed.
- **Embeddable `._pth` and `sitecustomize.py`**: embeddable Python skips
  `site` (and thus `sitecustomize.py`) unless `import site` is enabled in
  [`recipes/python/python314._pth`](../recipes/python/python314._pth). Enabling
  it leaves `sys.path` intact and includes user-site `%APPDATA%\Python\...`
  inside container data if created.
- **Shared `zipimporter` loader**: patching `exec_module` on
  `multiprocessing.connection`'s loader worked for unpacked files in PyGObject
  (one loader per module), but broke every subsequent standard-library import in
  embeddable Python where one `zipimporter` loads all modules in
  `python314.zip` (`multiprocessing.util` has no `arbitrary_address`).
  [`recipes/pygobject/sitecustomize.py`](../recipes/pygobject/sitecustomize.py)
  wraps the loader in a dedicated per-module wrapper instead.
- **Stage file replacement**: a non-`build` stage placing files into `B:\prefix`
  overwrites identically named files from earlier stages (`python314._pth`
  replacing the zip's copy).
- **Meld + MinGit**: Meld invokes Git with `STARTF_USESHOWWINDOW` so
  `pythonw.exe` flashes no console windows; with `mingit` in `from`, Git status
  and index refreshes in low-labelled working directories worked across all
  three sandboxes.

## Java

Measured 2026-10-19, Windows 11 26300,
originally probed Eclipse Temurin OpenJDK 25.0.4.1+1 and early-access 25.0.5+7
(current [`recipes/java-sdk.json`](../recipes/java-sdk.json) and
[`recipes/java.json`](../recipes/java.json) ship `25.0.5_8`, with
`java-sdk` setting `"sandbox": "soft"`).

Probed with `java Probe.java` on JDK and compiled bytecode on JRE:

| | soft | low | appcontainer |
|---|---|---|---|
| `user.home`, default | real profile (`SHGetKnownFolderPath(FOLDERID_Profile)`, `java_props_md.c`) | same, read-only | same, unreadable |
| `user.home` with `JAVA_TOOL_OPTIONS="-Duser.home=..."` (quoted for spaces) | container `home` | the same | the same |
| `java.io.tmpdir` | container `TEMP` | the same | container AppContainer `Temp` |
| `java.util.prefs` (`HKCU\Software\JavaSoft\Prefs`) | real registry | `BackingStoreException` ("Could not create windows registry node") | `SecurityException` (root key denied) |
| `hsperfdata` (`jps`, `jcmd` discovery) | in `TEMP` | in `TEMP` | not created |
| Swing window drawing text, self-closing | ok, Direct3D | ok, Direct3D | ok, Direct3D |

- **Bundled C runtime and module counts**: Temurin JDK/JRE zips ship
  `vcruntime140.dll`, `vcruntime140_1.dll`, `msvcp140.dll`, `ucrtbase.dll`, and
  `api-ms-win-*` forwarders (no host VC++ redistributable needed). The JRE
  ships 42 modules (`java.desktop`, `jdk.unsupported`, `jdk.localedata`...)
  and the JDK ships 62 (adding `jdk.compiler`, etc.).
- **`Path.toRealPath()` in an AppContainer**: Windows JDK walks from the drive
  root downward using `FindFirstFile`, failing even on `C:\Windows` in an
  AppContainer because drive roots deny listing:
  - *JDK 25 `conf\security\java.security` (JDK-8352728)*: failed all TLS,
    signing, hashing, and `Files.createTempFile` (`SecureRandom`) calls in
    25.0.4.1 (`Error loading java.security file`); fixed in 25.0.5.
  - *HarfBuzz FFM layout in 25.0.5+*: resolving `ucrtbase.dll` via `toRealPath`
    still throws `ExceptionInInitializerError` in `HBShaper` (breaking PlantUML
    class diagrams in AppContainer). Setting `-Dsun.font.layout.ffm=false`
    selects the working JNI shaper; both Java images set it.
- **`File.getCanonicalPath()` on `subst` drives**: resolves `B:` to the
  underlying staging directory while `toRealPath` and `getAbsolutePath` preserve
  `B:`. Maven's `-pl app` module selector compares canonical paths against the
  reactor and fails ("Could not find the selected project in the reactor";
  `-pl :pdfbox-app` works), and Gradle reports emit real paths; neither alters
  reproducible build outputs.
- **Direct `java.exe` entrypoints**: Maven 3.10 requires
  `maven.multiModuleProjectDirectory` (set by `mvn.cmd`), and Gradle 9.8.1's
  `gradle.bat` starts `gradle-cli-main` with an instrumentation agent.
  [`recipes/maven.json`](../recipes/maven.json) and
  [`recipes/gradle.json`](../recipes/gradle.json) use array `entrypoint`s
  launching `${java}\bin\java.exe` directly rather than wrapping `.cmd`/`.bat`
  scripts.
- **Vendoring Maven**: running
  the build into a local repository inside the module and deleting
  `_remote.repositories`, `*.lastUpdated`, `resolver-status.properties`,
  `maven-metadata-*.xml`, *and* `.meta/prefixes-*.txt` (which Central updates
  across time) yields deterministic offline Maven repositories (PDFBox: 90 s
  vendor, 40 s offline build).
- **Logisim-evolution offline Gradle build**: Kotlin DSL,
  configuration cache enabled, three portal plugins (Shadow 9.6.1, SonarQube
  7.5, Versions 0.61), Checkstyle resolved at config time, and an auxiliary
  source set produced 350 vendored files and bit-identical offline builds
  between stores after pinning `genBuildInfo`'s date to the tag commit in UTC.
- **Vendoring Gradle**: copying Gradle's `caches/modules-2/files-2.1` cache
  into Maven layout via [`recipes/gradle/GradleVendor.java`](../recipes/gradle/GradleVendor.java)
  and redirecting repositories with `QUASIDOCK_GRADLE_REPO` /
  [`recipes/gradle/quasidock-offline.gradle`](../recipes/gradle/quasidock-offline.gradle)
  handles settings plugins, `git-properties`, TeaVM, and `kotlin-dsl` offline
  (PlantUML: 3 min vendor, 2 min build).
- **PlantUML's bundled Graphviz**: `plantuml.jar` unpacks `graphviz.dat` (a
  small `dot` 2.44.1 Windows executable) into `%TEMP%\_graphviz` unless
  `GRAPHVIZ_DOT` or `PATH` overrides it; local source builds leave `$version$`
  until `patchCompilationInfo` fills it.
- **Two HKCU persistence bugs fixed on deep Java preference trees**
    (fixed in [`internal/hkcu/hkcu.go`](../internal/hkcu/hkcu.go)):
  - *Cleanup deleting pre-existing empty parents*: PDFBox uses
    `Software\JavaSoft\Prefs\org\apache\pdfbox`, and cleanup walked up to
    `Software` deleting every empty ancestor including a pre-existing empty
    `HKCU\Software\JavaSoft\Prefs`. Recording `OutermostMissing` stops
    `DeleteEmptyParents` before pre-existing keys.
  - *AppContainer traversal of `HKCU` ancestors*: Java opens each key from
    `Software\JavaSoft\Prefs` down before accessing its app subkey, failing with
    `SecurityException` unless granted. `grantAbove` grants the AppContainer
    capability `KEY_READ` (`NO_INHERITANCE`) on each ancestor key individually
    and revokes it on exit.
- **Apache mirrors vs Maven Central**: `archive.apache.org`,
  `downloads.apache.org`, and `repository.apache.org` timed out in local
  tests while `dlcdn.apache.org` and Maven Central succeeded; Maven and PDFBox
  recipes download from Maven Central (Apache parent POMs still incur snapshot
  timeouts on `repository.apache.org`).
- **JavaFX in Scene Builder**:
  - Temurin's JRE omits `jdk.unsupported.desktop` required by `javafx.swing`
    (`FindException` before `main`), so Scene Builder uses `java-sdk` (88 MB
    vs 55 MB JRE).
  - `maven-compiler-plugin` 3.13.0 bundles ASM 9.7 (supporting up to Java 23,
    failing Java 24+ `module-info.class` with `Unsupported class file major
    version 68`); 3.14.1 updates to ASM 9.8.
  - JavaFX loads native DLLs from `..\bin` beside its jars before unpacking into
    `user.home\.openjfx\cache`; Gluon's SDK avoids runtime cache extraction,
    and Direct3D 9Ex works in `soft`, `low`, and `appcontainer`.
- **Maven time-zone non-determinism**:
  In early builds, Scene Builder matched locally in UTC+7 yet differed from
  CI runners in UTC because `maven-shade-plugin` 3.2.4 rewrites ZIP entries
  carrying UTC extended timestamps (Woodstox, `stax2`, MSV, JAXB)
  in the JVM's local time zone
  (shifting 1,224 of 6,237 entries by 7 hours), ignoring
  `project.build.outputTimestamp` before 3.3.0. Passing
  `JDK_JAVA_OPTIONS=-Duser.timezone=UTC` matches CI bit for bit.

## Other findings

- **Fixed: `soft` runs could modify installed apps.** Python wrote a file into
  its shared deployed layer, breaking the image digest. `acl.Protect` in
  [internal/acl/acl.go](../internal/acl/acl.go) adds a deny-write-and-delete
  entry for the current user on each deployed layer, blocking any process
  running as that user inside or outside Quasidock until Quasidock unprotects
  and deletes the deployment (`rmi` / `prune`).

- **Apps don't expect the network to be denied.** BusyBox `wget` crashes with an
  access violation (`0xC0000005`) instead of reporting an error.

- **Folder redirection held.** `zig`, `node`, `npm`, `python`, and `git` all
  took their cache and config locations from the redirected environment
  variables rather than bypassing them with `SHGetKnownFolderPath`.

- **Grants are fast.** Measured during early development, granting read access
  to `zig`'s 19,546 files took 2.7 s once per app; grants on project folders
  took milliseconds.

- **Big builds and unpacks are limited by Defender scanning new files.**
  Unpacking `zig` (19.5k files, 378 MB) to disk and reading every file back took
  60–175 s because Defender scanned each new file before the readback, versus
  0.4 s to hash the layer. Streaming sources into the tar/layer writer and
  extracting deployments with 4 parallel workers reduced `zig` install to ~16 s
  and rebuilds to ~4 s with byte-identical layers.

## GTK's first run in CI

Measured on 2026-11-12:

- **GTK's family reproduces on a CI runner**: `gtk4-sdk`, `gtk4`, `gtk3`,
  `pygobject`, `gnome-text-editor`, `rnote`, `meld`, `gaphor`, and `gnumeric`
  built to the published layers in CI.

- **Recipe files need fixed line endings.** An image records the hash of each
  source file its recipe carries, so a runner checkout with
  `core.autocrlf=true` checking out `recipes/rnote/*` or
  `recipes/gnome-text-editor/*` with CRLF produced different configs.
  [.gitattributes](../.gitattributes) pins `recipes/** text=auto eol=lf` (as
  well as `internal/winres/testdata/* text eol=lf`, `LICENSE`, and `*.ps1`).

- **`zig`'s local cache made `cargo-c` build two images.** BLAKE3's build
  script enables AVX-512 (`-mavx512f -mavx512vl`) only if `cc` prints no
  warnings on a test compile. `-D__MSVCRT_VERSION__=0x700` triggered a
  redefinition warning from Clang on the first two builds, disabling AVX-512;
  on the third build in the same store, `zig` served both test compiles from
  its local cache without printing warnings, turning AVX-512 on and adding
  17 KB of code. Giving each stage a fresh `ZIG_LOCAL_CACHE_DIR`
  (`B:\src\<stage>\.zig-cache`, set in
  [internal/builder/builder.go](../internal/builder/builder.go)) keeps C
  runtime hits in `ZIG_GLOBAL_CACHE_DIR` (1.2 s link vs 124 s cold) while
  re-running compiler probes consistently.

- **Windows Server 2025 AppContainer `getpath`.** In an AppContainer on
  Server 2025, CPython's `getpath` cannot resolve its executable's real path
  (`self-path-dos`), warns, and continues.

## CMake's search paths, and Qt Quick

Measured on 2026-11-12, Windows 11 26300, 16 CPUs:

- **CMake searches `Program Files` and the registry on Windows.** Kitware's
  `Modules/Platform/WindowsPaths.cmake` adds `ProgramW6432`, `ProgramFiles`,
  `ProgramFiles(x86)`, and `%SystemDrive%\Program Files*` to
  `CMAKE_SYSTEM_PREFIX_PATH`. Even without `ProgramFiles` in the build env,
  `SystemDrive` caused `FindPostgreSQL` to pick up a host PostgreSQL install's
  MSVC `libpq.a` and `FindGit` to invoke host `git.exe`. Meanwhile `FindPython`
  defaults `Python_FIND_REGISTRY=FIRST`, reading
  `HKCU\Software\Python\PythonCore` before `PATH`.

- **`qtdeclarative` needs Python for build.** Yarr's `create_regex_tables` runs
  during configure (`os` and `sys` only); [recipes/qt6-sdk.json](../recipes/qt6-sdk.json)
  unpacks Python's embeddable zip and passes `-DPython_EXECUTABLE=...`.

- **`qtdeclarative`'s `qmlcachegen` races type registration.** `qmlcachegen`
  compiles QML bindings to C++ only when other modules' `plugins.qmltypes` are
  already written, which Ninja does not serialize cleanly. Two builds differed
  in the FluentWinUI3 style DLL (`455,168`, `462,336`, `457,216` bytes: 67 vs
  71 of 130 functions compiled). Even building `all_qmltyperegistrations` first
  still runs some `qmlcachegen` early; deleting `*/.rcc/qmlcache/*.cpp` and
  `*.aotstats` after the first build pass and re-running Ninja guarantees
  identical output ([recipes/qt6-sdk.json](../recipes/qt6-sdk.json)).

- **Qt Quick renders via Direct3D 11 in all three sandboxes.** Even in an
  AppContainer, HLSL shaders compile via the system `d3dcompiler_47.dll` (the
  Controls style on Windows 11 defaults to `"Windows"`).

- **Linguist runtime dependencies.** Linguist links `Qt6UiTools.dll` and
  `Qt6QuickWidgets.dll`, so [recipes/qt6.json](../recipes/qt6.json) retains
  `Qt6UiTools.dll` in the runtime image.

- **Notes 2.3.1 private modules and bitmap fonts.** Qt 6.10 stopped exporting
  private targets automatically; [recipes/notes.json](../recipes/notes.json)
  adds `CorePrivate GuiPrivate WidgetsPrivate` to `find_package(Qt6 ...)`. Its
  `#pragma comment(lib, "Dwmapi.lib")` works with lld; its stylesheets name
  `"MS Sans Serif"`, a bitmap font DirectWrite fails to load in all sandboxes.

## TeX Live and Ghostscript

Found on 2026-11-19, when TeX Live first built with `zig`:

- **Offline hermetic `install-tl`.** With a local CTAN snapshot (`-repository`
  containing `tlpkg/texlive.tlpdb` and `archive/`), `instopt_portable 1`,
  `TEXLIVE_INSTALL_NO_DISKCHECK=1` (avoids calling `powershell` not on build
  `PATH`), and `collection-wintools 0` in the profile, `install-tl` runs offline
  without touching the registry (1m48s measured for `scheme-small`; see
  [recipes.md](recipes.md#tex-live) and
  [recipes/texlive/texlive.profile](../recipes/texlive/texlive.profile)).

- **TeX formats are not reproducible.** Even under `SOURCE_DATE_EPOCH` and
  `FORCE_SOURCE_DATE=1`, pdfTeX embeds `\pdfrandomseed` from the clock, Knuth
  TeX embeds `\time`, LuaTeX embeds local `\time` and megabytes of differing
  state, and `ls-R` captures temp directory listings. `recipes/texlive` sets
  `tlpdbopt_create_formats 0` so formats are generated in container data on
  first run.

- **`mktexfmt.exe` vs `fmtutil.pl`, and Strawberry Perl.** Windows
  `mktexfmt.exe` (W32TeX) writes stdout to stderr, making `kpathsea` miss the
  created format path on the first run; invoking `fmtutil.pl` under the name
  `mktexfmt` prints the path, though Perl's `-w` only inspects the DOS
  read-only attribute instead of NTFS ACLs (whereas TeX Live's `dir_writable`
  writes a probe file). Strawberry Perl 5.42 lacks `Win32::Shortcut`, breaking
  `TLWinGoo.pm` callers (`fmtutil`, `updmap`, `tlmgr`), whereas `latexmk` does
  not load it.

- **XeTeX fontconfig and AppContainer `GetLongPathNameW`.** Post-install writes
  absolute `B:/prefix/...` paths into `fonts.conf`; rewriting them to
  `<dir prefix="relative">` and `WINDOWSFONTDIR` plus setting `XE_FC_CACHEDIR`
  makes XeTeX relocatable. However, `kpathsea` calls `GetLongPathNameW` on its
  own executable path at startup, which AppContainers deny (error on walking
  from the drive root, where `GetFinalPathNameByHandleW` succeeds), so TeX
  Live fails inside an AppContainer.

- **Ghostscript 10.08.0.** [recipes/ghostscript.json](../recipes/ghostscript.json)
  builds from upstream's release tarball with `zig:0.17.0` and BusyBox `make`
  with three fixes: stripping GNU `|` order-only prerequisites that Ghostscript
  emitted despite rejecting BusyBox `make`, removing unsupported `-Wp,-w`, and
  avoiding `mkromfs.c`'s conflicting `gp_stat_impl` stub.

- **Parallel builds serialize on `B:`.** Every build acquires the mutex
  `Local\quasidock-build-drive` ([internal/builder/drive.go](../internal/builder/drive.go)),
  printing `waiting for another quasidock build to finish with B:...` if
  another Quasidock build holds `B:`.

## The OCI tools

Probed on 2026-10-29, Go 1.27.1, cgo off against local `zot` via [tests/oci.sh](../tests/oci.sh)
(recipes updated to Go 1.27.3 in [recipes/oras.json](../recipes/oras.json),
[recipes/skopeo.json](../recipes/skopeo.json), and
[recipes/cosign.json](../recipes/cosign.json)):

- **All three build for Windows unchanged.** `skopeo` (`go.podman.io/skopeo`)
  uses build tags `exclude_graphdriver_btrfs containers_image_openpgp`.
  `cosign` requires Go 1.27 (`go 1.27` in `go.mod`; ~106 MB stripped).

- **`go mod vendor` size.** : ORAS 12 MB (772 files), skopeo
  46 MB (32 MB after pruning to `go list -deps`), cosign 130 MB / 10,554 files
  (103 MB pruned, pulling AWS/Azure/GCP/Vault KMS SDKs).

- **skopeo policy and auth paths.** Requires a policy at
  `CONTAINERS_POLICY_JSON`, `--policy`, `%APPDATA%\containers\policy.json`, or
  `C:\ProgramData\containers\policy.json`. Logins are stored in
  `%USERPROFILE%\.config\containers\auth.json`.

- **skopeo sees Quasidock images as OCI artifacts.** Because Quasidock's
  config media type is `application/vnd.quasidock.app.config.v2+json`,
  `skopeo inspect` fails with `"unsupported image-specific operation on artifact"`;
   `inspect --raw`, `list-tags`, and `copy` work.

- **skopeo's `oci:` transport on Windows.** Its regex
  (`^([a-zA-Z]:\\.+?):(.*)$`) only splits a `:tag` after a drive root `X:\`, so
  `oci:layout:1` fails as a path with a colon while `oci:layout` and
  `oci:D:\...\layout:1` work.

- **cosign 3 offline signing.** `cosign` 3 refuses `--tlog-upload=false`
  unless `--use-signing-config=false` is passed (otherwise querying Sigstore
  TUF), or unless given an empty config from
  `cosign signing-config create --out x.json`. `verify --key ...
  --insecure-ignore-tlog` works offline. Signatures signed natively by `cosign`
  default to OCI referrers rather than `sha256-<hex>.sig` tags.

- **AppContainer `filepath.EvalSymlinks` failure.** Writing outputs via
  ORAS's file store (`checkSymlinkEscape`) or skopeo's `oci:`/`dir:` transports
  calls `filepath.EvalSymlinks`, which walks directories from the drive root and
  fails with `Access is denied` inside an AppContainer (`oras push`, remote
  registry commands, and `cosign` key reads work).

## The small chains on Zig 0.17

Measured on Windows 11 26300 (16 logical processors, 16 GB RAM, 18.5 GB commit limit):

- **Zig 0.17's `cc` commits ~2x virtual memory when linking:** 906 MB vs 0.16's
  425 MB for a hello-world link (`zig cc -s -Wl,-Brepro hi.o`, independent of
  `-fuse-ld=lld` or CPU affinity; compiling a C++ TU is ~184 MB for zig and
  ~230 MB for clang). With 18 Ninja link jobs running concurrently, large C++
  SDKs (`gdal-sdk`, `postgresql-sdk`) hit `LLVM ERROR: out of memory` / paging
  file exhaustion, motivating their move to `llvm-mingw`.

- **`zig` compiler-rt lacks `__cpu_model`.** `__builtin_cpu_supports("avx2")`
  fails to link, whereas `llvm-mingw` includes it.

- **Clang 22 treats `-Wincompatible-pointer-types` as an error.** Passing
  `int *` for `DWORD *` (MSVC C4133 warning) failed under Clang 22 until
  [recipes/php-sdk/php-ext.php](../recipes/php-sdk/php-ext.php) added
  `-Wno-error=incompatible-pointer-types`. (`meson`, `make`, `cargo-c`,
  `ghostscript`, and `php-sdk` build cleanly on `zig:0.17.0`; note standalone
  PHP extension recipes were later removed in Quasidock.)

## Links, and what runs reach

Tested in [tests/reach.sh](../tests/reach.sh).

### A `quasidock` started from inside a run

A nested `quasidock` started inside a running app inherits that run's
redirected `USERPROFILE`, `LOCALAPPDATA`, and `TEMP`:

| What it read | From inside a run | What went wrong |
|---|---|---|
| `SHGetKnownFolderPath(Documents)` | first app's home | wrong profile path |
| `USERPROFILE` | first app's home | mistaken for real profile (made read-only under `low`) |
| `LOCALAPPDATA` | first app's home | default store not recognized |

When `QUASIDOCK_ID` is present in the environment, `Run` in
[internal/cli/cli.go](../internal/cli/cli.go) calls `environment.RestoreUser`
([internal/environment/environment.go](../internal/environment/environment.go)),
which reads `CreateEnvironmentBlock` for the user's real variables and applies
them in-process too, keeping `QUASIDOCK_*` variables intact.

Because `--sandbox=low` and `--sandbox=appcontainer` cannot open the store, a
run that needs to launch host programs outside the store uses **`--share=host`**
(available under `soft` and `low`; refused/no host directory under
`appcontainer` in [internal/run/run.go](../internal/run/run.go)). It mounts
[quasidock-host.exe](../cmd/quasidock-host/main.go), which restores the real
user environment (`CreateEnvironmentBlock` / `os.Args`) while keeping the
caller's security token and integrity level (`low` in a `low` run; checked in
[tests/reach.sh](../tests/reach.sh)).

## Upstream versions

[scripts/upstream.txt](../scripts/upstream.txt) (129 rows) and
[scripts/check-upstream.sh](../scripts/check-upstream.sh) track upstream
releases and OSV advisories:

- **Tags and directory listings often look like newer releases:** python.org
  creates `3.16.0/` directories for alphas, libjpeg-turbo `3.1.90` is a 3.2
  beta, Pango `1.90.0` was an abandoned Pango 2 snapshot, PlantUML `v8059`
  predates date-based tags, `jdk-25+36` sorts poorly against `jdk-25.0.5+8`,
  and Ghostscript `gs100501` (10.05.1) sorts inconsistently against `gs10080`
  (10.08.0). Checking that the pinned version matches on the page catches bad
  patterns, not upstream numbering quirks.
- **PHP and Node bundled libraries:** PHP's download directory is ahead of
  `packages-8.5-vs17-x64-stable.txt`; a `-1` suffix (such as `8.5.12-1`) is a
  library rebuild where `php -v` still prints `8.5.12`. Similarly, Node
  cherry-picked the Brotli CVE-2026-111497 fix into 24.22.2 without bumping
  `process.versions.brotli` from `1.2.0`.

### Advisories

Added on 2026-11-05 in [scripts/check-upstream.sh](../scripts/check-upstream.sh)
via OSV's `/v1/querybatch` and `/v1/vulns/<id>` endpoints:

- **OSV's `GIT` ecosystem matches repositories and tags,** covering C libraries
  with no package registry, but only against the canonical URL OSV indexes
  (e.g., GLib's GitLab repo had 14 vs GitHub mirror 0; VLC 21 vs 3; Ghostscript
  197 vs 0).
- **A zero result does not prove coverage (`osv-thin`).** Comparing recent vs
  older tags on 2026-11-05 showed `v24.0.0` (Node: 18), `openssl-3.5.0` (61),
  `version-3.50.0` (SQLite: 18), `v3.14.7` (Python: 21), whereas `php-8.4.0`,
  `REL_16_0` (PostgreSQL), `v6.5.0` (Qt), and `2.80.0` (GLib) all returned `0`—even
  for PHP versions with known critical RCEs. `scripts/upstream.txt` marks such
  upstreams `osv-thin`.
- **Aliases and CVSS filtering.** Multiple OSV IDs often collapse to fewer
  advisories via `aliases`. Because CVSS "High" impact includes availability
  (DoS), `check-upstream.sh` flags vectors matching `/AV:N/`, `/PR:N/`,
  `/UI:N`, and `/V?[CI]:H` (confidentiality/integrity). Go advisories omit CVSS
  vectors and show as `"not stated"`.
- **`go version -m` dependencies:** rebuilt Go
  binaries had 0 stdlib advisories, while vendored modules still matched
  OSV (`zot` 2.1.23: 5 across 175 modules; `cosign` 3.1.4: 5 across 256; `oras`
  1.3.6: 1 across 23; `skopeo` 1.24.1: 20 across 87; `fzf` 0.74.5: 3 across
  11), though unreached code requires `govulncheck` to assess.
- **Zero false reassurance & false positives:** `glib-networking` 2.90.0 showed
  `0` in OSV before 2.90.1 shipped 5 CVEs including `CVE-2026-113622`;
  conversely, `libjpeg-turbo` 3.1.4.1 triggered `OSV-2026-1068` despite bad
  commit ranges and affecting only disabled `-DWITH_TURBOJPEG=OFF` APIs.

## A connection reset while downloading

In Quasidock's codebase:

- **Blob downloads resume on connection resets.** In
  [internal/registry/registry.go](../internal/registry/registry.go), `GetBlob`
  sets `req.Resumable = true`. When `exchange` hits a transport or body-read
  error (`Client.Do` or `io.Copy`), it records `r.Broken = err` and returns
  `ErrBroken`. If at least 1 byte was received (`cw.n > 0`) and attempts are
  within `MaxResumes` (`5`), `GetBlob` logs `resuming <short> at <at> bytes,
  after <err>` under `--verbose` and retries with `Range: bytes=<at>-...`
  (and `downloadTo` retries once if a server ignores `Range` with `200 OK`).
- **Manifest fetches fail immediately.** `FetchManifest` / `FetchManifestRef`
  do not set `Resumable`, so a mid-manifest reset returns `GET <url>: <err>`.
- **Source fetches fall back to registry-stored sources.**
  [internal/builder/fetch.go](../internal/builder/fetch.go) downloads sources
  with `http.Get` and falls back to `remote.FetchSource` (sources pushed next
  to the image) on any download error or SHA-256 mismatch.
- **Verification.** Both `QUASIDOCK_TEST_DOWNLOAD_BREAK` (simulated breaks)
  and [tests/cmd/resetproxy/main.go](../tests/cmd/resetproxy/main.go) (which
  forwards `<bytes>`, sleeps 500 ms, sets `tcp.SetLinger(0)`, and closes to emit
  a real RST) exercise blob resets, manifest resets, and source fallbacks in
  [tests/registry.sh](../tests/registry.sh).

## As a real standard user

Tested via [tests/standard-user.sh](../tests/standard-user.sh) and
[tests/cmd/asuser/main.go](../tests/cmd/asuser/main.go) (`CreateProcessWithLogon`
with `LOGON_WITH_PROFILE`, verifying the token is absent from `S-1-5-32-544`,
runs at Medium Mandatory Level, and fails `net session`) on Windows 11 26300:

- **Core workflow works without admin rights.** `scripts/defender-scan.sh`,
  `scripts/install.ps1` (including user `PATH` and uninstalling
  `HKCU\Software\Quasidock\Grants`), `pull` from `ghcr.io`, `soft`, `low`, and
  `appcontainer` runs, `grants --take-back`, `--filesystem` on an owned folder
  on another drive, `--share=network` (AppContainer loopback exemption: 2–7 ms
  add, ~2 ms remove), `build --rebuild busybox.json` on `B:`, Credential
  Manager (`logout`), and suites `store`, `sandbox`, `batch`, `mounts`,
  `ctrlc`, `network`, `reach`, `install`, and `build-base` (except MSVC) work
  as a non-admin standard user.
- **Modify-only folders not owned by the user.** On a drive root giving `Users`
  only Modify (not WritePermissions/owner), `icacls /setintegritylevel` fails
  even for an unelevated administrator unless the user owns the directory;
  unowned Modify-only directories are refused cleanly with an explanation
  ([gaps.md](gaps.md)).
- **WebCache (`dllhost.exe`) on MSVC builds.** In an account nobody has
  interactively signed into, an MSVC build starts Windows' WebCache under the
  build's redirected profile and leaves files in `WebCache` locked after exit
  until cleaned up by `prune`.
- **BusyBox `sh -c` last-command wait bug.** Running `sh -c 'cat ...'` in an
  AppContainer exposed BusyBox's quirk of not waiting on its last non-builtin
  command.
- **Protected deployments.** `acl.Protect` ([internal/acl/acl.go](../internal/acl/acl.go))
  denies owner delete to standard and filtered-admin users alike; `rm -rf` on a
  store fails unless `rmi`/`prune` or `icacls /reset /T` runs first.

## Signing with cosign

Evaluated using `cosign`3.1.4 directly from the store
(see [scripts/sign-lib.sh](../scripts/sign-lib.sh),
[scripts/sign-images.sh](../scripts/sign-images.sh), and
[tests/signing.sh](../tests/signing.sh)):

- **Offline `sign-blob` flags.** With `COSIGN_PASSWORD` set, `cosign sign-blob`
  defaults to Sigstore bundles and TUF lookups; emitting a raw signature
  offline requires `--new-bundle-format=false --use-signing-config=false
  --tlog-upload=false --yes --output-signature`.
- **Signature and key encoding.** `sign-blob` emits base64-encoded ASN.1 DER
  ECDSA P-256 over SHA-256, verified directly by
  [internal/signature/signature.go](../internal/signature/signature.go)
  (`x509.ParsePKIXPublicKey` on the 91-byte PKIX public key, `ecdsa.VerifyASN1`).
  `cosign generate-key-pair` produces `ENCRYPTED SIGSTORE PRIVATE KEY` (P-256).
- **Classic `.sig` tags vs `cosign verify`.** Quasidock pushes and verifies
  `sha256-<hex>.sig` tags (`quasidock signature payload`, `push`, `check`),
  which stock `cosign verify --key ... --insecure-ignore-tlog` also reads.
  However, `cosign verify` does not check the payload's `docker-reference`
  against the requested image repository, so a signature copied from another
  repository passes `cosign verify`. Quasidock verifies JSON duplicate keys,
  `PayloadType`, `Digest`, and case-insensitive `Repository` equality
  ([internal/signature/signature.go](../internal/signature/signature.go),
  [how-it-works.md](how-it-works.md#signatures)), and scopes its single
  embedded key ([internal/trust/quasidock.pub](../internal/trust/quasidock.pub))
  to `ghcr.io/quasidock` ([internal/trust/trust.go](../internal/trust/trust.go)).
- **MSYS2 `curl` trap in test scripts.** Under `MSYS_NO_PATHCONV=1`,
  `curl -o /dev/null` treats `/dev/null` as a Windows path and fails; use
  `-o NUL` with `-f`.

## An empty macro in BusyBox's `make`

Discovered when `enchant` failed intermittently in CI on `make -C lib install nodist_doc_DATA=`:

- **`update_makeflags()` off-by-one.** In BusyBox-w32 `FRP-6075`'s
  `miscutils/make.c`, `update_makeflags()` allocates
  `strlen(name) + 2 * strlen(value) + 1` zeroed bytes per CLI macro. With an
  empty value, `=` occupies the final byte, leaving no NUL terminator so
  `xappendword()` appends adjacent heap bytes into `MAKEFLAGS` passed to
  recursive `make` calls (and blanks-only values fail similarly because escaping
  consumes the spare bytes).
- **Reproduced on demand (100 runs each):** `a234567=` leaked heap garbage into
  `MAKEFLAGS` in 100/100 runs, `a2345678901234567890123=` in 98/100, and
  `nodist_doc_DATA=` 0/100 in a tiny Makefile despite failing in larger CI
  builds (`enchant` had `4f034a22` from BusyBox's store path on its heap);
  non-empty values (`=x`) or setting the macro inside the `Makefile` failed
  0/50 times.
- **Still present upstream.** BusyBox `FRP-6354` / master and standalone
  `pdpmake` share the same calculation (needs `+ 2`).
  [recipes/gtk4-sdk.json](../recipes/gtk4-sdk.json) empties `nodist_doc_DATA =`
  directly in `enchant`'s `lib/Makefile`; recipes that require GNU `make`
  (such as `pgxs` extensions) can use [recipes/make.json](../recipes/make.json).

## `zlib` inside images

Measured by scanning deployed binaries for `inflate 1.x Copyright` strings
(note that standalone`postgis` was later removed from `recipes/`):

| Image | File | zlib |
|---|---|---|
| `gtk4` / GTK apps | `bin/libz.dll` | **1.4** |
| `postgresql` | `postgres.exe`, tools, `pgcrypto.dll` | **1.4** |
| `postgresql` | bundled Perl's `zlib1__.dll` | 1.3.1 |
| `postgresql` | bundled Perl's `parl.exe`, `parldyn.exe`, `Zlib.xs.dll` | 1.3.2 |
| `osm2pgsql`, `sqlite` | `osm2pgsql.exe`, `sqlite3.exe` | **1.4** |
| `gdal` | `libgdal-39.dll` (GDAL's internal copy) | 1.3.1 |
| `gaphor` | Pillow `_imaging` / `_imagingft` wheels | 1.3.1 |
| `mingit`, `meld` | `zlib1.dll`, `msys-z.dll` | 1.3.2 |
| `mingit`, `meld` | `libSkiaSharp.dll` | 1.2.13.1 |
| `qt6`, `flameshot`, `notes` | `Qt6Core.dll` (Qt's internal copy) | 1.3.2 |
| `php` | `php8.dll`, `php_zip.dll`, `libssh2.dll` | 1.3.2 |
| `node` | `node.exe` | 1.3.0.1 & 1.3.2.1 |

Build-time only tools: `rust`'s `ld.exe`/`dlltool.exe` (1.2.13), `cmake`
(1.3.1), `zig.exe` (1.3.1), and `perl`. Note that absence of the copyright
string does not prove absence of zlib (e.g., Python, FreeType's `src/gzip`, or
Rust deflate crates), and presence does not imply whether `gzwrite` CVEs such
as `CVE-2026-76844` or `CVE-2026-85091` are reachable.

**`z_off_t` in zlib 1.4:** zlib 1.4 reverted `z_off_t` to 32-bit on Windows
unless `_FILE_OFFSET_BITS=64` is defined; in the audited builds, `osm2pgsql`'s
`libosmium` sets `_FILE_OFFSET_BITS=64` for `gzoffset64`, while PostgreSQL
only calls `gzopen`/`gzread`/`gzwrite`.

## Lazy running with the Cloud Files API (experiment, 2026-11-04)

From the initial Cloud Files prototype ([cloud-files.md](cloud-files.md)):

- **Works unprivileged under all three sandboxes.** Hydrating and executing
  dehydrated placeholders works with `acl.Protect` and sandbox grants under
  `soft`, `low`, and `appcontainer`. Dev Drives refuse it (`478`, filter
  cannot attach).
- **Placeholder creation is ~10x faster than full unpack** (0.55 s vs 5.8 s
  for `llvm-mingw`), advancing first use by 1.7–8x when fetching 1 MiB blocks;
  per-file HTTP requests lose to full pulls.
- **Granularity and overheads.** A plain Windows file open hydrates the whole
  file (lazy granularity is per file, not per page); first opens pay ~7.5 ms
  extra (Defender scan on demand rather than during extract).
- **Hydrated DLL image section cost.** Creating a memory section for a hydrated
  77 MB DLL took 60 ms on every launch vs 0.5 ms on a plain file even after
  unregistration/reversion; swapping in a plain unpacked copy eliminates it.
- **Provider process restrictions.** One provider connects per sync root; a
  dehydrated `.exe`/`.dll` without a running provider fails with
  `BAD_EXE_FORMAT`, and the provider's own process cannot open or launch its
  own placeholders (`395`).

## Lazy pulling

### Status in Quasidock

Quasidock retains `pull --lazy` (and dropped `update --lazy` when self-update
was removed):

- [internal/lazy/lazy.go](../internal/lazy/lazy.go) checks eligibility
  (`DefaultMinSize` 32 MiB, override `QUASIDOCK_TEST_LAZY_MIN`, eStargz TOC
  chunk digests present, server honors HTTP `Range`), writes
  `lazy/<hex>/{tail,layer.json,state.json}`, acquires a shared `pull` lock, and
  spawns `<store>/lazy/quasidock-<hash16>.exe lazy-helper` detached.
- [internal/lazy/blobreader.go](../internal/lazy/blobreader.go) implements the
  sparse blob reader (`1 MiB` blocks, map magic `"QDBLOB01"`, `4 MiB` demand
  pieces, `8 MiB` background pieces, `1000 ms` quiet window after demand
  reads) with `Ensure`, `Prefetch`, and `DownloadAll`.
- [internal/cloudfiles/cloudfiles.go](../internal/cloudfiles/cloudfiles.go)
  dynamically loads `cldapi.dll` (`CfRegisterSyncRoot`, `CfUnregisterSyncRoot`,
  `CfCreatePlaceholders`, `CfHydratePlaceholder`, `CfExecute`,
  `CfReportProviderProgress`, `CfGetSyncRootInfoByPath`, `CfConnectSyncRoot`,
  `CfDisconnectSyncRoot`) and maps Win32 errors `1`, `50`, `379`, `381`, and
  `478` (Dev Drive) to `ErrUnsupported`.
- [internal/lazy/helper.go](../internal/lazy/helper.go) currently registers the
  sync root, creates placeholders, signals `ready`, waits for the `pull` lock
  to release, downloads the full blob via `DownloadAll`, verifies SHA-256,
  extracts with 4 workers, and swaps into `deploy/<hex>` once `DeploymentInUse`
  clears. Note that connecting `CfConnectSyncRoot` callbacks and calling
  `Hydrate` on `estargz.Prioritized()` files (and the `QUASIDOCK_TEST_LAZY_*`
  gate/fail/refuse test hooks expected by [tests/lazy.sh](../tests/lazy.sh))
  are not yet wired into `helper.go` in the Go port.

## Not yet tested

- **Live Ctrl+C console testing:** [tests/ctrlc.sh](../tests/ctrlc.sh) builds
  `node` and runs [tests/cmd/ctrlc/main.go](../tests/cmd/ctrlc/main.go), whose
  `CreatePseudoConsole` helpers are implemented while `main()` currently only
  initializes and exits `0` ([iteration 2](iteration-2.md)), leaving live
  interactive Ctrl+C delivery untested end to end.
- **Tools with fixed install paths or installers.**
