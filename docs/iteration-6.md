# Iteration 6: Container lifecycle, per-container isolation, and detached runs

Quasidock **0.6.0** adds Docker-style container records, per-container isolated runtime data, detached execution, container management commands, and `--rm` in place of `--ephemeral`.

## Outcome

- Added container commands: `container ls` / `ps` (`-a`/`--all`, `-q`/`--quiet`, combined `-aq`/`-qa`), `container inspect` (`-f`/`--format`), `container logs` / `logs` (`-f`/`--follow`, `-n`/`--tail`), `container stop` / `stop` (`-t`/`--time`), `container rm` / `rm` (`-f`/`--force`), and `container prune`.
- Updated top-level `inspect` to resolve containers first and fall back to cached images; `image inspect` remains image-only.
- Added `run` flags `--name NAME`, `--rm`, `-d`/`--detach`, `-i`/`--interactive`, `-t`/`--tty`, and combined short boolean flag clusters before `IMAGE`. Retired `--ephemeral` with an error pointing to `--rm`, and replaced the `0.5` `rm` -> `rmi` deprecation hint with container removal (`rmi` / `image rm` remains for images).
- Added per-container storage under `<store>/containers/<id>/` (`state.json`, `container.log`, `run.lock`, `data/`) and catalog management in `internal/store/containers.go` (`ContainerRecord`, lock-backed status reconciliation, short/full ID and name resolution, validation, and pruning).
- Isolated mutable state per container: `${data}`, `HOME`, `APPDATA`, `LOCALAPPDATA`, `TEMP`, cache, and the HKCU hive now target `<store>/containers/<id>/data` instead of shared `<store>/data/<app-id>`. Multiple containers — including different versions of the same app — can run concurrently without conflicting.
- Retained exited containers after both foreground and detached runs finish so `ps -a`, `inspect`, and `logs` remain available until removed. Passing `--rm` removes the container directory, its data, and HKCU leftovers on normal or stopped exit.
- Added detached supervision via the internal `container-supervisor` command (`StartDetached` with `CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | CREATE_UNICODE_ENVIRONMENT` and ready/stop named events) and cooperative shutdown (`StopContainer` signaling the stop event, sending `CTRL_BREAK_EVENT`, and terminating the Job Object after the grace window).
- Pinned images referenced by both running and exited containers against `rmi` (removing secondary aliases still succeeds; removing the last reference requires removing referencing containers first). Updated `quasidock prune` to collect stopped containers and settle HKCU leftovers before pruning newly unpinned unnamed images and deployments.
- Migrated `scripts/images.sh`, `tests/cmd/clifixture`, Go CLI/store integration suites, shell E2E tests, and `README.md` to `0.6.0`. Fixed `qd_remove_app` in `scripts/images.sh` (`{{.AppID}}` template field and exit-status preservation) caught during installer testing.

## Decisions and limits

- Keep foreground launches in the caller's process group so host Ctrl+C delivery (`tests/ctrlc.sh`) continues to work, while allocating a new process group (`CREATE_NEW_PROCESS_GROUP`) for detached runs so `stop` can raise `CTRL_BREAK_EVENT`.
- Accept and store `-t`/`--tty` (`Config.Tty`) while using pipe-backed streams (`\\.\NUL` stdin unless foreground `-i`). ConPTY allocation, bind mounts (`-v`/`--mount`), Docker-style `-p` port publishing, Dockerfiles, and the Docker Engine API remain deferred.
- `stdout` and `stderr` pipes are drained concurrently into `container.log`, so interleaved output ordering across streams is unserialized per line.
- Leave existing `<store>/data/<app-id>` directories from earlier versions untouched; clean unused legacy directories with `quasidock prune --data`.

## Validation

| Check | Result |
|---|---|
| `go test -count=1 ./...` | All 41 test packages pass (`internal/cli`, `internal/run`, `internal/store`, `internal/lifecycle`, host/relay commands, and helpers) |
| `bash -n`, `scripts/*.sh` and `tests/*.sh` | All scripts pass (`syntax ok`) |
| `tests/batch.sh` | 11 checks pass (`0 check(s) failed.`) |
| `tests/reach.sh` | 8 checks pass (`0 check(s) failed.`) |
| `tests/rebuild.sh` | 25 checks pass (`0 check(s) failed.`) |
| `scripts/package.sh` | Packaged `0.6.0` zip, installer, `SHA256SUMS`, and `notes.md` in `out/release` |
| `tests/install.sh` (`PACKAGE_DIR=out\release`) | 15 checks pass (`0 check(s) failed.`) |
| `scripts/defender-scan.sh out/release` | All three packaged binaries scan clean (`ok`) |

`go run ./scripts/build.go all argv-echo hostprobe ctrlc-driver cli-fixture` and `tests/upstream.sh` (`154 row(s) cover the sources of 85 recipe(s); 0 problem(s)`) also passed prior to the `scripts/images.sh` helper adjustment; `scripts/package.sh` built the stripped release binaries tested above.

The final stripped binaries and release artifacts in `out/release`:

| File | SHA-256 |
|---|---|
| `quasidock.exe` | `5828b94b38a4c8ab7bf211eb121d3bec07d070272186ec6f163eaa2d89e6bb02` |
| `quasidock-relay.exe` | `2ee81cc238a8b7fe25a24e116e3fb16a8ec7e2b2b304675e3268532b2adcadf9` |
| `quasidock-host.exe` | `0bf497082bbe3f0a675c50e0cc10b7bc040dc5b886a8c1c25ed8f861583508ed` |
| `quasidock-x86_64-windows.zip` | `6a132c2988123f23908dae6aeb3e1b12c7d0a3cc0a4d184ec703cf11c91dbce2` |
| `install.ps1` | `f95ffc0120505296da181de43494071b727f36bd3bb74069224ccee6af668c81` |

## Not exercised here

Download-dependent, external-registry, toolchain, and separate standard-user-account shell suites (`tests/build.sh`, `tests/php.sh`, `tests/published.sh`, `tests/store.sh`, `tests/sandbox.sh`, and registry-seeded suites) were updated where needed and syntax-checked with `bash -n`, without live network seed downloads. Loopback registry and local fixture integration tests in `internal/cli` cover container lifecycle, stream/log handling, `--rm`, stop/grace timeouts, pinning, and pruning locally.
