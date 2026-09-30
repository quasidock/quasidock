# Iteration 2: Rebrand, slimming down, and green E2E

Started after the Go rewrite of Iteration 1. Quasidock is **0.2.0**, hard-forked from Zigsaw under `github.com/quasidock/quasidock` with a clean runtime break (`QUASIDOCK_*`, `%LOCALAPPDATA%\quasidock`, `quasidock.exe`, `quasidock-relay.exe`, `quasidock-host.exe`), while keeping wire compatibility with `ghcr.io/zigsaw-io` published images.

## Outcome

- **Rebrand across code, resources, workflows, scripts, and tests**:
  - Go module and all Go packages moved from `github.com/zigsaw-io/zigsaw` to `github.com/quasidock/quasidock` (`0` remaining `zigsaw-io/zigsaw` imports).
  - Windows manifest/resource files in `src/windows/quasidock.{rc,manifest}`, `.gitattributes`, `scripts/build.go`, all `.github/workflows/{release,reproduce}.yml`, `scripts/*`, and `tests/*` updated for `quasidock` and `0.2.0`.
  - Compatibility kept for `oci.DefaultRegistry` (`ghcr.io/zigsaw-io`), `trust.PublishedPrefix` (`ghcr.io/zigsaw-io`), legacy media types (`application/vnd.zigsaw.*` alongside `application/vnd.quasidock.*`), `recipes/no-publish/files.json` (`Files - Zigsaw build`), and `ZIGSAW_GRADLE_REPO` (`recipes/gradle/zigsaw-offline.gradle`).
- **Removed desktop integration**:
  - Dropped `internal/shortcuts`, `internal/associations`, `internal/exports`, `internal/aliases`, `internal/shim`, `internal/sidecar`, `internal/pe`, and `cmd/zigsaw-shim*`, plus `--share=commands` / `--unshare=commands`.
  - Kept backwards-compatible JSON fields (`Exports`, `Aliases`, `Shortcuts`, `Associations`, `Schemes`, `Permissions.Commands`) ignored on read.
  - Deleted desktop test suites `tests/shims.sh`, `tests/guiapps.sh`, and `tests/gui/`.
- **Removed self-update and rollback machinery**:
  - Dropped `update`, `update --check`, `updates`, `upgrade`, `rollback`, `hold`, `unhold`, `prune --previous`, `internal/schedule`, `internal/updates`, `cmd/zigsaw-notify`, and `store/versions`.
  - Adapted `tests/registry.sh` eStargz delta tests (`sgz_delta`, `sgz_mismatch`) to use `pull --yes` and deleted `tests/updates.sh`, `tests/next-release.sh`, `tests/rollback.sh`, and `tests/self.sh`.
- **Fixed four regressions uncovered during end-to-end verification**:
  - [`cmd/quasidock-host/main.go`](../cmd/quasidock-host/main.go): switched from `win32.CommandLineToArgvW` (which loaded `shell32.dll` and panicked inside `--sandbox=low`) to `os.Args`, and aligned batch line-break error wording with [`internal/run/run.go`](../internal/run/run.go).
  - [`tests/cmd/argv/main.go`](../tests/cmd/argv/main.go): switched argument reading (`ucrtArgs`) to `ucrtbase.dll` (`_configure_wide_argv`, `__p___argc`, `__p___wargv`) so the test printer matches Windows UCRT parsing (`""` inside quotes stays quoted, unlike `CommandLineToArgvW`/`os.Args`'s 3k+1 rule on `""""`).
  - [`internal/cli/cli.go`](../internal/cli/cli.go): `overrideApp` now calls `run.WarnUnsupported` when saving a `--sandbox=...` override on an app that declares that sandbox unsupported; added [`TestOverrideWarnsUnsupported`](../internal/cli/cli_test.go).
  - [`internal/estargz/estargz_test.go`](../internal/estargz/estargz_test.go): sorted `buildSampleTar` keys and used deterministic incompressible bytes in `TestPlanDeltaUpdate` to eliminate a flaky map-order/tiny-compressed-gap test failure under `-count=1`.
  - [`tests/rebuild.sh`](../tests/rebuild.sh): updated its fake binary env var from `ZIGSAW` to `QUASIDOCK`.

## Decisions

- **Keep `ghcr.io/zigsaw-io` and legacy OCI media types**: existing published images and embedded trust keys remain pullable without republishing everything first.
- **No backward-compatibility shim for local `ZIGSAW_*` stores or registry keys**: Iteration 2 starts cleanly with `QUASIDOCK_HOME`, `QUASIDOCK_GRANTS_KEY`, and `%LOCALAPPDATA%\quasidock`.
- **Preserve commented-out workflow steps**: kept commented lines intact in `.github/workflows/{release,reproduce}.yml`.
- **Use UCRT in `tests/cmd/argv`**: batch escaping (`BuildBatchCommandLine`) escapes `"` as `""` to preserve `cmd.exe` quote balance across arguments while matching MSVC/UCRT consumers (`node.exe`, `python.exe`, C/Rust/Zig binaries).

## Results

| What | Result |
|---|---|
| `go build ./...` | exits 0 |
| `go test -count=1 ./...` | 39 test packages (`cmd/quasidock-host`, `cmd/quasidock-relay`, 36 `internal/*` packages, `tests/cmd/p256sign`) pass (`5/5` uncached runs on `internal/estargz`) |
| `bash -n` on all `.sh` files | 47 scripts (`scripts/*.sh` and `tests/*.sh`) pass syntax check |
| `tests/batch.sh` | 11 ok, 0 failed |
| `tests/reach.sh` | 9 ok, 0 failed |
| `tests/install.sh` | 15 ok, 0 failed |
| `tests/upstream.sh` | 51 ok, 0 failed (`154 row(s) cover the sources of 85 recipe(s); 0 problem(s)`) |
| `tests/rebuild.sh` | 29 ok, 0 failed |
| `scripts/package.sh` + `scripts/defender-scan.sh` | packaged `0.2.0` (`quasidock-x86_64-windows.zip`, `install.ps1`, `SHA256SUMS`); Defender scan clean on all 3 binaries (`quasidock.exe`, `quasidock-relay.exe`, `quasidock-host.exe`) |
| `tests/ctrlc.sh` | exits 0 (builds `recipes/node.json`; note `tests/cmd/ctrlc/main.go` is still a stub printing `ctrlc test driver initialized successfully`) |

`0.2.0` binaries built in `zig-out/bin/`:

| Program | SHA-256 |
|---|---|
| `quasidock.exe` | `770BAB4E82687ECC875208CF234D20B196E1C59D8E19C36C1A7523BB2C7E44ED` |
| `quasidock-relay.exe` | `E8AF565D83AB6411F911ED0E1F358B5A95B06DC2E85AADC3C982BCE11B76A7C7` |
| `quasidock-host.exe` | `AFFC37ECC53FC95CC514C41BC260C19BFBD5BA20808E84F0CF8E3621AEE75493` |

## What went wrong

- **`shell32.dll` init panic under `--sandbox=low`**: `quasidock-host` panicked on `CommandLineToArgvW` inside low-integrity runs (`Failed to load shell32.dll: A dynamic link library (DLL) initialization routine failed`). Fixed by using `os.Args`.
- **`tests/batch.sh` quote mismatch with Go `os.Args`**: Go's `os.Args` implements Win32 `CommandLineToArgvW` (`3k+1` quote bug splitting `""""`), unlike UCRT/Zig `argv`. Switched `tests/cmd/argv` to `ucrtbase.dll` wide argv parsing and matched the `ErrInvalidBatchArgument` refusal message in `internal/run/run.go` and `cmd/quasidock-host/main.go`.
- **Silent `--sandbox` override on unsupported images**: `overrideApp` saved overrides without running `run.WarnUnsupported`. Restored the warning.
- **`tests/rebuild.sh` missed `QUASIDOCK` env var**: still exported `ZIGSAW` for the fake binary, causing builds to invoke the real CLI instead of the fake script. Fixed in `tests/rebuild.sh`.
- **External `frippery.org` HTTP 403**: `recipes/busybox.json` could not be downloaded from `https://frippery.org/files/busybox/busybox-w64-FRP-6075-g169694ebd.exe` (`HTTP 403 Forbidden`, verified with `curl.exe` as well), blocking busybox-dependent end-to-end suites on this machine without a seeded download cache.

## What was not checked

- **Busybox-dependent E2E suites** (`tests/sandbox.sh`, `tests/store.sh`, `tests/build.sh`): blocked by upstream `frippery.org` returning HTTP 403 Forbidden (no local cached `07bb1e5b...` busybox binary on this host).
- **Local registry, signing, OCI, lazy, published, standard-user, and toolchain suites** (`tests/zot.sh`, `tests/registry.sh`, `tests/signing.sh`, `tests/oci.sh`, `tests/lazy.sh`, `tests/published.sh`, `tests/standard-user.sh`, `tests/network.sh`, `tests/files.sh`, `tests/chromium.sh`, `tests/gtk.sh`, `tests/gtk3.sh`, `tests/qt.sh`, `tests/java.sh`, `tests/texlive.sh`, `tests/zls.sh`, `tests/acprobe.sh`): not run in this pass.
- **GitHub Actions YAML parsing**: PyYAML is not installed locally, so `.github/workflows/{release,reproduce}.yml` syntax was checked by inspection and will be exercised on push.
