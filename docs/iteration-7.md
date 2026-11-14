# Iteration 7: Persistent storage and data mounts

Quasidock **0.7.0** adds reusable named volumes and writable bind mounts of local
host directories at `${data}` or its subpaths. Containers can share mounted
storage across applications, versions, sandbox modes, and stores.

## Outcome

- Added `volume create NAME`, `volume ls [-q]`, `volume inspect [-f TEMPLATE]`,
  `volume rm`, and `volume prune [--dry-run]`. Names resolve case-insensitively;
  creation is idempotent. Volume metadata and contents live separately under
  `volumes/<name>/volume.json` and `volumes/<name>/data`.
- Added repeatable `run -v` / `--volume SOURCE:TARGET[:rw]` and
  `--mount type=volume|bind,source=SOURCE,target=TARGET`. Missing named volumes
  are created automatically. Bind sources must exist; relative paths resolve
  in the caller's working directory before detached supervision.
- Added normalized `HostConfig.Mounts` and resolved inspection `Mounts` containing
  `Type`, optional volume `Name`, `Source`, `Destination`, and `RW`. Existing
  container records default to no mounts; recipes and OCI configs are unchanged.
- Added `internal/mounts` for parsing, validation, canonical paths resolved from
  opened Windows objects, NTFS directory junctions, and deletion/size accounting
  that never traverse mounted contents. Mounts are attached before creating
  profile, cache, or registry directories.
- Serialized volume references and removal with the container catalog. Running,
  exited, and failed-launch container records pin their named volumes. Container
  removal, `--rm`, ordinary pruning, and image removal preserve mounted data;
  only explicit volume removal/pruning collects unreferenced named volumes.
- Added process-backed mount permission ownership in the user's grant ledger.
  Concurrent consumers preserve one another's AppContainer capabilities and
  integrity labels, including overlapping source directories across stores.
  Cleanup restores the original explicit permission masks and integrity labels.
  Ordinary filesystem grants, image cleanup, `grants`, and `repair` participate
  in shared ownership checks.
- Made HKCU storage identity follow the actual backing registry directory.
  Runtime locks remain outside mounted data; ownership records retain every
  participating store's lock and app identity. Mounted registry state survives
  `--rm`; incompatible backing hives still conflict. Corrupt hives and failed
  saves now report errors instead of silently replacing persisted state.
- Fixed two lifecycle issues exposed by the new tests: stopping/removing an
  auto-removed container succeeds when its final record has already disappeared,
  and removing an image tolerates absent service-port metadata.
- Added fixture-backed CLI tests and an offline `tests/mounts.sh` suite. Extended
  Windows CI and the separate standard-user suite, updated script option helpers,
  and documented storage syntax, cleanup, and upgrading in README and recipe docs.

## Decisions and limits

- Support writable directories on local NTFS volumes. Targets must be `${data}`
  or data subpaths; multiple targets must be disjoint. Reject read-only mounts,
  anonymous volumes, file mounts, remote shares, drivers, and arbitrary absolute
  destinations. No image-directory copy-up occurs.
- Canonicalize sources before checking protected store, installation, and
  profile-root paths. Named-volume contents are the managed store exception.
  Reject reparse points in destination ancestry and managed volume directories.
- Keep state, logs, and runtime locks outside mounted data. Registry backing
  directories are preserved when mounted; unmounted container data remains
  private and retains its previous lifecycle.
- Applications coordinate concurrent file writes. Sharing storage does not
  remove the existing Windows sandbox or HKCU-key limitations.
- Existing 0.6 containers require no migration. Use 0.7 or later to manage
  containers with mounts; older versions do not understand their metadata or
  shared permission ownership.

## Validation

| Check | Result |
|---|---|
| `go test -count=1 ./...`, normal Windows user context | All 42 test packages pass |
| Final HKCU/CLI regression checks | Pass, including relative detached binds, shared consumers, supervisor death, mounted hives, corrupt hive preservation, and `--rm` stop |
| `bash -n` on `scripts/*.sh` and `tests/*.sh` | All scripts pass |
| `tests/mounts.sh` | 12 checks pass, including all three sandboxes |
| `tests/mounts.sh` against the extracted final release zip | 12 checks pass |
| `tests/batch.sh` | 11 checks pass |
| `tests/reach.sh` | 8 checks pass |
| `tests/rebuild.sh` | 31 checks pass |
| Production binaries and local test helpers | Build successfully |
| `scripts/package.sh` | Generates the 0.7.0 zip, installer, checksums, and release notes |
| `tests/install.sh`, using the final package | 15 checks pass |
| `scripts/defender-scan.sh out/release` | All three packaged binaries scan clean |

Store and permission tests also cover concurrent volume removal/reference
creation, case-insensitive names, retained/stale container pinning, non-mutating
dry runs, source sentinels surviving cleanup, nested shared-source permissions,
restoration of preexisting access, and recovery from dead owners.

The final release artifacts in `out/release`:

| File | SHA-256 |
|---|---|
| `quasidock.exe` | `db176b4059e8b7a7905ce365b8f8516fe0ea7af83843780b2f3dc89ec8f4e346` |
| `quasidock-relay.exe` | `ccadd5fef3f9fac3ce259b2a526e0a92a321b38f500281e8ec9040c15cc041ce` |
| `quasidock-host.exe` | `28beb58ad1b60542e4a758ee4a4e6a0ad05a259c1aa20a90144133f0edaba34d` |
| `quasidock-x86_64-windows.zip` | `1894b0ea81659e29268f8c2f03affc04aa3f90cd2ee9d420e4b4297fef459fe1` |
| `install.ps1` | `75b2319c905e70800dc62e817c16708a240f5858e1712d741e68dfe35109c7f0` |

## Not exercised locally

The separate standard-user-account suite is configured to include mount coverage
but was not run locally. External-registry, downloaded-toolchain, and GUI suites
were not run; local fixtures and loopback registries cover the changed contracts.
The restricted execution context initially denied access to test-created temporary
directories; Windows integration checks were completed in the normal user context.
