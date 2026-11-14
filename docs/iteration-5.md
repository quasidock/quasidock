# Iteration 5: Docker-style image management and foreground run

Quasidock **0.5.0** introduces a breaking CLI cleanup, multiple cached versions
per app, and foreground execution from image references with automatic pulling.
Published Quasidock/Zigsaw image formats and existing app data remain compatible.

## Outcome

- Added `image ls`, `inspect`, `pull`, `tag`, `push`, and `rm`, with short aliases
  `images`, `inspect`, `pull`, `tag`, `push`, and `rmi`. Inspect provides JSON and
  Go templates; image IDs are full manifest digests.
- Removed top-level `list`, app-uninstall `rm`, remote `info`/`ls`/`cat`,
  `run --command`, two-argument push, and verbosity alias `-v`. Errors identify
  replacements. Remote operations use `image info`, `image files`, and `image cat`.
- Added versioned `images/index.json` with independent manifests, canonical
  names, repository provenance, and app-default pointers. Updates use an advisory
  lock and atomic replacement. Tag changes preserve previous manifests.
- Added one-time migration of legacy refs. It validates image metadata, requires
  existing runs to finish, preserves files/data, and never reimports retained refs.
- Builds and pulls retain earlier versions. Bare IDs select the last successful
  build/pull; explicit references and digests select exact images, including after
  tag movement. Loading, publishing, layer reuse, and GC use the whole catalog.
- Added `--pull=missing|always|never`, defaulting to `missing`, and `-e`/`--env`
  inheritance, removal, and last-value precedence. Run options stop at IMAGE.
- Mapped image `command` plus `args` to default CMD. Positional commands replace
  both; `--entrypoint` clears CMD while retaining export argument prefixes and
  Windows batch quoting.
- Added process-backed app/manifest leases. The same manifest may run
  concurrently; another version cannot overlap. Launch failure or owner death
  releases leases, and stale state cannot block subsequent runs.
- Kept per-app data and `--ephemeral`. Removal refuses active images, preserves
  shared layers, and cleans app-wide grants/profiles only after the final cached
  version is removed. In-use deployments retain their AppContainer ACLs when
  another image is loaded. Data is collected separately with `prune --data`.
- Fixed fetch prerequisites: persist original manifest bytes, read downloaded
  config before moving its temporary file, accept full digests in blob-presence
  checks, and check signatures before downloading rejected images. Failed
  refreshes preserve existing references/defaults. Lazy pull uses the verified
  path through a layer-preparation callback.
- Migrated active scripts and the installer to inspect queries and new commands.
  App suites use `scripts/images.sh` to explicitly select their intended
  entrypoint; direct CLI integration tests independently exercise CMD replacement.
- Added a loopback registry fixture, local test executable, and Windows CI job
  for catalog and foreground CLI integration tests.

## Decisions and limits

- Keep existing registry defaults and media types. App IDs remain Quasidock
  shortcuts; Docker Hub name expansion is not introduced.
- Preserve recipe builds, publishing option forms, sandbox options, services,
  trust, grants, and repair. No image-config schema or recipe hashes change.
- Full manifest IDs select local images; pulling needs a repository reference.
  Failed `--pull=always` never falls back to stale content.
- Keep per-app persistent data across versions. Named containers, detached
  execution, Dockerfiles, Engine API compatibility, interactive flags, mounts,
  and Docker-style port publishing remain deferred.
- Use 0.5 after store migration: retained legacy refs are no longer updated.

## Validation

| Check | Result |
|---|---|
| `go test -count=1 ./...`, normal Windows user context | All 41 test packages pass |
| Local registry/foreground integration | Pass: multiple versions, exact selection, tag/push/pull, offline use, pull policies, failed refreshes, signature outcomes, command/entrypoint, environment, batch quoting, exit status, data, shared layers, conflicts, and crash recovery |
| Catalog tests | Pass: one-time/idle-only migration, concurrent writers, aliases, removal protection, stale leases, and non-mutating dry runs |
| `go run ./scripts/build.go all argv-echo hostprobe ctrlc-driver cli-fixture` | Three production binaries and four requested helpers built |
| `bash -n`, `scripts/*.sh` and `tests/*.sh` | All 47 scripts pass |
| `tests/rebuild.sh` | 31 checks pass |
| `tests/batch.sh` | 11 checks pass, including low-integrity and AppContainer |
| `tests/reach.sh` | 8 checks pass |
| `scripts/package.sh` | Packaged 0.5.0 zip, installer, checksums, and release notes |
| `tests/install.sh`, against the package | 15 checks pass |
| `scripts/defender-scan.sh out/release` | All three packaged binaries scan clean |

The final stripped binaries in `out/release/quasidock-x86_64-windows.zip`:

| Program | SHA-256 |
|---|---|
| `quasidock.exe` | `37f12f956c88df3f71b062699c7256e2ca6ff8fab62ebc09557b42dfbe6a4328` |
| `quasidock-relay.exe` | `fb600d5cb798633e35ccaf45b51c7c3bf32e493393030965bb1b2346618c7780` |
| `quasidock-host.exe` | `f955a5130f92603b1c07b05ee877f7f36c55cdc3bd1a32fcad8598434de6ac39` |

The restricted execution context reproduced baseline AppContainer/HKCU failures.
The full Go suite and sandbox E2E checks pass in the normal Windows user context.

## Not exercised here

External-registry, BusyBox/download-dependent, GUI/toolchain, and separate
standard-user-account suites were migrated and syntax-checked, but not run live.
The loopback tests exercise the changed contracts without those dependencies.
The existing Ctrl+C driver still only checks initialization; repairing it remains
outside this iteration.
