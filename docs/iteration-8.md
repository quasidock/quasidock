# Iteration 8: Publish Quasidock images on GHCR

Quasidock **0.8.0** adds a manually dispatched image publishing workflow for the
67 distributable recipes and changes the default registry to `ghcr.io/quasidock/quasidock`.

## Implementation

- Generate dependency-aware Windows recipe jobs from the publish allowlist. Build
  Quasidock once and share its binaries across the run.
- Bootstrap into candidate tags and produce a reviewable dependency-pin patch.
  Publish committed pins only after candidate signature, source, and smoke checks;
  update application tags before replacing the complete catalog.
- Use repository-token authentication and a dedicated Quasidock P-256 signing
  key scoped to `ghcr.io/quasidock/quasidock`. Remove the upstream built-in trust keys.
- Complete source artifact upload and hash-checked download/vendor fallback. Emit
  valid OCI manifests for sources, catalogs, and signatures.
- Order SDKs and runtime layers deterministically, and resolve existing deployed
  runtime directories by digest instead of depending on Go map iteration.
- Preserve explicit registry overrides, cached selections, containers, and store
  formats. No recipe schema or public application commands are added.

## Setup and rollout

See [publishing.md](publishing.md) for key setup, bootstrap, public package
settings, publication, retry behavior, and release checks.

The public key is intentionally unconfigured until signing setup. A maintainer
must commit the public key, configure the two signing secrets, run bootstrap,
review and commit its generated pins, make packages public, and complete publish
and Reproduce before releasing 0.8. No images or GitHub settings were changed by
the local implementation.

## Validation

| Check | Result |
|---|---|
| `go test -count=1 ./...`, normal Windows user context | All 44 test packages pass |
| Source and vendor fallback integration | Pass, including restoration of stage inputs after a failed vendor command |
| Production binaries and publishing helper | Build; CLI reports 0.8.0 |
| Actionlint 1.7.12, new and changed workflows | Pass |
| Generated workflow and shell syntax checks | Pass |
| `tests/batch.sh` | 11 checks pass |
| `tests/reach.sh` | 8 checks pass |
| `tests/mounts.sh` | 12 checks pass |
| `tests/rebuild.sh` | 31 checks pass |
| `tests/publish.sh` | 4 checks pass; no registry or credentials required |

The restricted execution context reproduced the existing AppContainer/HKCU
permission failures; the complete suite passed in the normal user context.
Live GHCR publication, the full downloaded-application builds, and independent
Reproduce remain part of the configured rollout.
