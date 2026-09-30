# Iteration 4: Idiomatic Go build conventions and non-interactive `pull`

Quasidock **0.4.0** removes the remaining Zig wrapper shims, Zig build path/flag conventions, and legacy `src/` layout left over after the Zig-to-Go port, and makes `quasidock pull` non-interactive by default (matching `docker pull`).

## Outcome

- **Removed Zig shims and legacy `src/` layout**:
  - Deleted root wrapper scripts (`zig`, `zig.cmd`, `build.bat`) and [`scripts/build.sh`](../scripts/build.sh).
  - Relocated active Go test fixtures from `src/` into package-local `testdata/` directories: `internal/zipfile/testdata/sample.zip` (updated [`internal/zipfile/zipfile_test.go`](../internal/zipfile/zipfile_test.go)) and `internal/winres/testdata/quasidock.manifest` (updated [`internal/winres/winres_test.go`](../internal/winres/winres_test.go)).
  - Removed unreferenced legacy files (`src/windows/quasidock.rc`, `src/testdata/sample.tar.gz`, `src/testdata/sample.tar.xz`, `src/testdata/cosign/*`) and eliminated the top-level `src/` directory.

- **Migrated build conventions to idiomatic Go**:
  - Updated [`scripts/build.go`](../scripts/build.go): changed the default output prefix from `zig-out` to `out` (`out/bin/*.exe`, `out/test/*.exe`), standardized the strip flag to `--strip`, and appended `-s -w` to `-ldflags` alongside `-trimpath` when `--strip` is set.
  - Updated [`.gitignore`](../.gitignore) (replaced `.zig-cache/` and `zig-out/` with `out/`), [`.gitattributes`](../.gitattributes) (replaced Zig and `src/` rules with Go and `internal/**/testdata/**` rules), [`README.md`](../README.md) (added `# Building`), and `MapMagic` in [`internal/lazy/blobreader.go`](../internal/lazy/blobreader.go) (`"ZIGBLOB1"` to `"QDBLOB01"`).
  - Replaced `zig-out` and `-Dstrip` references with `out` and `--strip` across [`.github/workflows/release.yml`](../.github/workflows/release.yml), [`.github/workflows/reproduce.yml`](../.github/workflows/reproduce.yml), 6 scripts in `scripts/` (`defender-scan.sh`, `package.sh`, `publish.sh`, `rebuild.sh`, `record-prefetch.sh`, `sign-images.sh`), 30 scripts in `tests/*.sh`, and 4 test recipe fixtures (`tests/batch/app.json`, `tests/hostprobe/app.json`, `tests/reach/outer.json`, `tests/regprobe/app.json`).
  - Updated [`tests/reach.sh`](../tests/reach.sh) to build both `argv-echo` and `hostprobe` at startup (matching `tests/reach/outer.json` and the `quasidock-hostprobe.exe` invocation), and updated [`tests/standard-user.sh`](../tests/standard-user.sh) to stage `out/bin` + `out/test` and provide a stand-in `go` stub in `$stage\bin\go`.

- **Made `quasidock pull` non-interactive by default**:
  - Updated [`remote.Fetch`](../internal/remote/remote.go) to print the app's name, download size, and permissions (`WriteName` / `WriteContents`) to stderr after `FetchInfo` and before `DownloadBlobs`, removing `Consent` and `AgreeTo`.
  - Factored [`parsePullOptions`](../internal/cli/cli.go) in [`internal/cli/cli.go`](../internal/cli/cli.go) and updated the `pull` help text (`pull [--lazy] <image>`); kept `--yes` accepted silently as a compatibility no-op.
  - Added [`TestParsePullOptions`](../internal/cli/cli_test.go) and updated [`tests/registry.sh`](../tests/registry.sh) to verify non-interactive `pull` with stdin closed (`</dev/null`) without `--yes`, while also verifying `--yes` remains accepted.

## Decisions

- **Keep Zig container recipe references untouched**: `ZIG_LOCAL_CACHE_DIR` in [`internal/builder/builder.go`](../internal/builder/builder.go) and `zig build ... -Dstrip` in [`recipes/zls.json`](../recipes/zls.json) configure the sandboxed `org.ziglang.zig` toolchain used when building third-party Zig apps, not Quasidock's own build.
- **Keep `--yes` as a no-op flag on `pull`**: Preserves backwards compatibility for scripts and test suites passing `quasidock pull --yes` while making `quasidock pull <image>` non-interactive by default.
- **Bump version to `0.4.0`**: Updated [`internal/version/version.go`](../internal/version/version.go).

## Results

| What | Result |
|---|---|
| `go run ./scripts/build.go all` + 15 test helpers | exits `0`; builds 3 production binaries in `out/bin/` and 15 helpers in `out/test/`; `quasidock.exe version` prints `quasidock 0.4.0` |
| `go test -count=1 ./...` | 41 test packages pass (`0` failed) |
| `bash -n` on all `.sh` files | all 46 scripts (`scripts/*.sh` and `tests/*.sh`) pass syntax check |
| `scripts/package.sh` + `scripts/defender-scan.sh out/release` | packaged `0.4.0` (`quasidock-x86_64-windows.zip`, `install.ps1`, `SHA256SUMS`, `notes.md`); Defender scan `ok` on `quasidock.exe`, `quasidock-relay.exe`, and `quasidock-host.exe` |
| `tests/batch.sh` | 11 ok, 0 failed |
| `tests/reach.sh` | 8 ok, 0 failed |
| `tests/install.sh` (`PACKAGE_DIR=out\release`) | 15 ok, 0 failed |
| `tests/rebuild.sh` | 31 ok, 0 failed |
| `tests/upstream.sh` | 68 ok, 0 failed (`154 row(s) cover the sources of 85 recipe(s); 0 problem(s)`) |

`0.4.0` stripped binaries (`go run ./scripts/build.go --strip`) packaged via `scripts/package.sh`:

| Program | SHA-256 |
|---|---|
| `quasidock.exe` | `8a7bb89d4ed728176cf289537a1e685cdb4456fc1c4b0e7733bfd47d3c8dedbe` |
| `quasidock-relay.exe` | `74e5d96bfc2a76ba2e54e67032bfe420316bb934a03584c790c64ae88f8e2f11` |
| `quasidock-host.exe` | `cbff1a70a5cdba2c53af2fcb1baab1690c3ad66c9f6321b17a15ebf80850bebb` |

## What was not checked

- **Registry-, BusyBox-, network-, and GUI-dependent E2E suites** (`tests/store.sh`, `tests/sandbox.sh`, `tests/build.sh`, `tests/network.sh`, `tests/ctrlc.sh`, `tests/zot.sh`, `tests/registry.sh`, `tests/signing.sh`, `tests/oci.sh`, `tests/lazy.sh`, `tests/published.sh`, `tests/standard-user.sh`, and app suites such as `tests/gtk.sh`, `tests/gtk3.sh`, `tests/qt.sh`, `tests/java.sh`, `tests/php.sh`, `tests/postgresql.sh`, `tests/chromium.sh`, `tests/files.sh`, `tests/matrix.sh`): validated via `bash -n` syntax checks rather than executed live in this pass.
