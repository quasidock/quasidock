# Iteration 3: Dropping overrides, extensions, user-folder junctions, and `--file`

Quasidock **0.3.0** removes the remaining Flatpak-style subsystems — persistent per-app and global overrides, extension points, named user-folder junction linking, and `--file` — while keeping store compatibility and validation of existing published images and recipes intact.

## Outcome

- **Removed persistent overrides (`override`)**:
  - Deleted `internal/override` (`override.go`, `override_test.go`), `"overrides"` from `subdirs` in [`internal/store/store.go`](../internal/store/store.go), and the `override` CLI subcommand (`overrideApp`, `settleGrants`, `case "override"`, and `--real-folders` / `--noreal-folders`) from [`internal/cli/cli.go`](../internal/cli/cli.go).
  - Moved runtime `Settings`, `SandboxOf`, `FilesystemFor` (with `mergeList`, `without`, `grantKey`) into [`internal/run/run.go`](../internal/run/run.go), and `ParsePort` into [`internal/services/services.go`](../internal/services/services.go).
  - Removed override indicators and cleanup calls from `list`, `remove`, [`internal/lifecycle/install.go`](../internal/lifecycle/install.go), and `run.Run` verbose output.

- **Removed extension points (`--extension`, `--noextension`)**:
  - Deleted `internal/extensions` (`extensions.go`, `extensions_test.go`), `--extension` / `--noextension` flags in [`internal/cli/cli.go`](../internal/cli/cli.go), and extension resolution/mounting/joining in [`internal/run/run.go`](../internal/run/run.go).
  - Removed `Join` and the extra join-vars map parameter from `Expand` in [`internal/environment/environment.go`](../internal/environment/environment.go), updating callers in [`internal/builder/builder.go`](../internal/builder/builder.go) and [`internal/run/run.go`](../internal/run/run.go).
  - Simplified `services.UsedBy` in [`internal/services/services.go`](../internal/services/services.go) to take only `cfg oci.AppConfig`.
  - Removed extension-point and `extends` display output from `cli.list`, [`internal/lifecycle/install.go`](../internal/lifecycle/install.go), [`internal/info/info.go`](../internal/info/info.go), and [`internal/remote/remote.go`](../internal/remote/remote.go).

- **Removed named user-folder junctions and `--file`**:
  - Deleted `internal/folders` (`folders.go`, `folders_test.go`), `QUASIDOCK_USER_FOLDERS` handling, `--file` option parsing and `openedFiles` in [`internal/cli/cli.go`](../internal/cli/cli.go) and [`internal/run/run.go`](../internal/run/run.go).
  - Restricted CLI `--filesystem` and `--nofilesystem` flags to `cwd` or an absolute path, rejecting `oci.FsGrantFolder` values (`desktop`, `documents`, `downloads`, `music`, `pictures`, `videos`) on the command line ([`TestParseRunOptionsRejectsRemovedFlags`](../internal/cli/cli_test.go)).

- **Cleaned orphaned test fixtures and updated shell suites**:
  - Removed unused `gui-fixture` from [`scripts/build.go`](../scripts/build.go), deleted `tests/cmd/gui/main.go`, and deleted 10 unused fixtures in `tests/reach/` (`docs.json`, `gui.json`, `hold.cmd`, `hold.json`, `real.json`, `rec.cmd`, `shared.json`, `tool.json`, `url.json`, `url2.json`), keeping `outer.json` and `echo.cmd`.
  - Updated [`scripts/record-prefetch.sh`](../scripts/record-prefetch.sh) and 17 scripts under `tests/*.sh` (`build.sh`, `chromium.sh`, `files.sh`, `gtk.sh`, `gtk3.sh`, `java.sh`, `lazy.sh`, `matrix.sh`, `network.sh`, `php.sh`, `postgresql.sh`, `published.sh`, `qt.sh`, `reach.sh`, `sandbox.sh`, `signing.sh`, `standard-user.sh`).

## Decisions

- **Preserve recipe and image schema compatibility**: Keep `Extensions` and `Extends` JSON fields in `oci.AppConfig`, `oci.Runtime`, and `recipe.Recipe` ignored at runtime, and keep `oci.ParseFsGrant` accepting `FsGrantFolder` while `run.Run`, `oci.DescribePermissions`, and `oci.AddedAccess` skip `FsGrantFolder` and `p.Commands` at runtime. Existing recipes and published images remain valid with unchanged recipe hashes.
- **Keep `--filesystem` strictly path-based on the CLI**: Users grant host directories explicitly via `--filesystem=cwd` or `--filesystem=<abs-path>[:ro]`; named user folders and `--file` are rejected at CLI parsing time.
- **Bump version to `0.3.0`**: Updated [`internal/version/version.go`](../internal/version/version.go) so compiled binaries report `quasidock 0.3.0`.

## Results

| What | Result |
|---|---|
| `go run ./scripts/build.go` + 14 test binaries | exits `0`; `quasidock.exe version` reports `quasidock 0.3.0` |
| `go test -count=1 ./...` | 41 test packages (`cmd/quasidock-host`, `cmd/quasidock-relay`, 38 `internal/*` packages, `tests/cmd/p256sign`) pass (`0` failed) |
| `bash -n` on all `.sh` files | all 47 scripts (`scripts/*.sh` and `tests/*.sh`) pass syntax check |
| `tests/batch.sh` | 11 ok, 0 failed |
| `tests/reach.sh` | 8 ok, 0 failed |
| `tests/install.sh` | 15 ok, 0 failed |
| `tests/upstream.sh` | 68 ok, 0 failed (`154 row(s) cover the sources of 85 recipe(s); 0 problem(s)`) |
| `tests/rebuild.sh` | 31 ok, 0 failed |
| `scripts/package.sh` + `scripts/defender-scan.sh` | packaged `0.3.0` (`quasidock-x86_64-windows.zip`, `install.ps1`, `SHA256SUMS`, `notes.md`); Defender scan clean (`ok`) on `quasidock.exe`, `quasidock-relay.exe`, and `quasidock-host.exe` |

`0.3.0` stripped binaries (`go run ./scripts/build.go -Dstrip`) packaged via `scripts/package.sh`:

| Program | SHA-256 |
|---|---|
| `quasidock.exe` | `1347c8fb23b03cfc650e60d932960cd661b1746b187b545f2311616c4c072193` |
| `quasidock-relay.exe` | `ae6bf70ddc732844a1b42a84786587079f9bfbb9db4d5576172358056a688f54` |
| `quasidock-host.exe` | `ddc445256be6995cf62cee28018435647c24531cda7770f2ab0823956ebcb006` |

## What was not checked

- **BusyBox- and network-dependent E2E suites** (`tests/sandbox.sh`, `tests/store.sh`, `tests/build.sh`, `tests/network.sh`, `tests/ctrlc.sh`, `tests/zot.sh`, `tests/registry.sh`, `tests/signing.sh`, `tests/oci.sh`, `tests/lazy.sh`, `tests/published.sh`, `tests/standard-user.sh`, and toolchain/GUI suites such as `tests/gtk.sh`, `tests/gtk3.sh`, `tests/qt.sh`, `tests/java.sh`, `tests/php.sh`, `tests/postgresql.sh`, `tests/chromium.sh`, `tests/files.sh`, `tests/matrix.sh`): not executed locally in this pass (validated via `bash -n` syntax checks). Note that `tests/reach.sh` expects an absolute binary path (or default relative path from repo root) because it `cd`s into a temporary directory before running `quasidock`.
