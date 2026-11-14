# Publishing images

The **Publish images** workflow publishes the 67 recipes in
`scripts/published-recipes.txt` to `ghcr.io/quasidock/quasidock`. A short pull such
as `quasidock pull node` resolves to `ghcr.io/quasidock/quasidock/node:latest`.
`QUASIDOCK_REGISTRY` overrides this namespace. Images in `recipes/no-publish`
are excluded. Run the workflow manually from `dev`; other branches and forks
cannot enter its publishing jobs. The binary Release workflow remains separate.

## One-time setup

1. Install Cosign 3.1.4 independently of Quasidock's registry. The CI installer
   uses the same version, which supports the existing P-256 signature format.
2. Generate a dedicated key **outside this repository** with
   `COSIGN=/path/to/cosign.exe bash scripts/signing-key.sh /path/outside/repo`.
   Keep the encrypted private key and passphrase backed up securely.
3. Copy only `cosign.pub` into `internal/trust/quasidock.pub`, preserving PEM
   format and a final newline. Commit the public key.
4. Configure repository Actions secrets `COSIGN_PRIVATE_KEY` (the complete
   encrypted `cosign.key` file) and `COSIGN_PASSWORD` (its passphrase). In Git Bash:

   ```bash
   gh secret set COSIGN_PRIVATE_KEY --repo quasidock/quasidock < /path/outside/repo/cosign.key
   gh secret set COSIGN_PASSWORD --repo quasidock/quasidock
   ```

   The second command prompts for the password. Do not commit either secret.
5. Ensure organization policy allows this repository's `GITHUB_TOKEN` to create
   and write container packages. CI uses the actor and repository token through
   Quasidock's credential environment variables; no registry PAT is required.
   Existing packages must grant `quasidock/quasidock` Actions access if they were
   created elsewhere.

GHCR links packages first published with a workflow repository token to that
repository. New packages are **private**, including when the repository is public;
permission inheritance does not change package visibility. See GitHub's
[GHCR authentication and repository linkage](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
and [package visibility](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility).

## Bootstrap a namespace or refresh dependency pins

BusyBox needs a hash-checked source seed before starting the full graph; see the
source seed procedure below. When the new namespace has no seed yet, CI can
recover the same binary from BusyBox's digest-pinned `source_mirrors`.
CI reads only that source into each build store and the temporary suites'
download caches, without requesting frippery.org or pulling the mirror's
executable layers.

Dispatch **Publish images**, choosing `bootstrap`:

```bash
gh workflow run publish.yml --repo quasidock/quasidock --ref dev -f mode=bootstrap
```

Each recipe job waits for its dependencies, copies the recipe tree into runner
temporary storage, updates its dependency pins from their verified build reports,
and builds the copied recipe. Builds never change the checked-out recipes or push
commits. The generated graph currently has nine dependency levels; independent
images build concurrently in separate Windows jobs.

Workers push `ghcr.io/quasidock/quasidock/APP:ci-RUN_ID`, source artifacts under
`sha256-DIGEST.sources`, and signatures under `sha256-DIGEST.sig`. They smoke-test
the candidates in fresh stores. Bootstrap verification uses the repository token
because the packages may still be private. A candidate catalog is published under
`catalog:ci-RUN_ID`; version tags, `latest`, and `catalog:latest` stay unchanged.

Download the `publishing-result` artifact. It contains `pins.patch`, `images.json`,
and `summary.md`; individual reports and build logs are retained separately.
Review the patch, apply it with `git apply --check pins.patch` followed by
`git apply pins.patch`, and commit it. These pins describe the new Quasidock images,
rather than assuming upstream manifests are available or compatible.

Open [the organization's package settings](https://github.com/orgs/quasidock/packages).
Make each of the 67 image packages and `catalog` public, and check their repository
association. This is an explicit maintainer setup step. Bootstrap only reports
the checklist; it does not claim that the candidates are publicly accessible.

## Publish committed recipes

After committing the bootstrap pins and making packages public, dispatch:

```bash
gh workflow run publish.yml --repo quasidock/quasidock --ref dev -f mode=publish
```

Publish mode builds committed recipes without changing pins. If a dependency's
result differs from a committed pin, it stops with instructions to bootstrap.
Each application is uploaded and signed as a candidate, then verified and
smoke-tested in a fresh store without registry credentials. Signing occurs only
after building, and the supplied key must match the bundled public key.

The final job requires a complete, consistent result set and anonymously verifies
all application signatures and source artifacts before promotion. It reuses exact
manifest bytes for version and `latest` tags, then publishes the complete catalog
last. Existing tag-update semantics remain: tags may move, digest references do
not. No timestamps or per-run annotations are added to application manifests.

Promotion across packages is not atomic. An interrupted promotion reports where
it stopped; rerun the failed jobs for the same source revision. Already-uploaded
blobs and signatures are reusable. Candidate tags and artifacts are retained for
inspection; this workflow does not delete package versions.

After publication, run **Reproduce** against the same revision. Its grouped builds
compare independent build digests with published images, and its suites exercise
the applications. Release 0.8 only after this verification and package setup.
The Release workflow checks the bundled key and anonymous published pins before
packaging the new default registry.

Reproduce's `pins` job runs before the downloaded-application jobs. A missing or
private release stops that job and leaves the Windows build and application suites
skipped; the local image and container integration job still runs independently.
A successful bootstrap only stages candidates: it does not populate version tags,
`latest`, or `catalog:latest`. Changing the registry prefix also requires bootstrap
and publication in the new namespace. A rerun of Reproduce cannot create those tags.

The dependency pins applied from bootstrap run `43670743979` describe the verified
Quasidock builds previously staged in `ghcr.io/quasidock`. Run bootstrap in
`ghcr.io/quasidock/quasidock`, review any new `pins.patch`, make its packages public,
and complete publish mode before running Reproduce against the new namespace.

## Local maintenance and verification

### Recipe migration

The migrated SDK recipes package native commands in `sdk-bin` and declare the
compilers needed to build them. Run bootstrap after these changes: existing
committed image digest pins describe the previous SDK images, and bootstrap
replaces them with verified results from the same run. Review and commit its
`pins.patch` before using publish mode.

All 16 vendor pins in the publish allowlist have been migrated to Quasidock's
canonical tar format. Each original archive was recovered from its legacy source
artifact and verified against its existing SHA-256, then the same paths and file
contents were repacked with the current writer. This changes archive metadata,
not dependency contents. The nine hashes observed in the failed bootstrap match
these independently computed hashes.

The PHP SDK also pins the reviewed replacement of
`libssh2-1.11.1-10-vs17-x64.zip` (`6d957ebde340...`). Compared with the original
hash-verified archive, its 17 file names, headers and import libraries are
unchanged. Five files changed: the DLL, PDB and three SBOM files. The replacement
still identifies libssh2 1.11.1; its SBOM records the upstream rebuild with zlib
1.4 in place of 1.3.2-1. Source hash verification remains mandatory.

The GLib and libenchant2 dependency archives were subsequently rebuilt against
the same zlib update. Their replacement pins were checked against the original
hash-verified archives: file names, headers and import libraries remain
unchanged; binaries, debug symbols and SBOM files changed. All 16 PHP SDK
downloads were checked together to avoid stopping on the next stale archive.

Meson's SDK explicitly names its native `pkg-config.exe` through `PKG_CONFIG`.
Perl's image removes Strawberry Perl's incompatible `pkg-config` scripts so they
cannot shadow the native tool in downstream builds. TeX Live retains its
original input set with a digest-pinned legacy source artifact as a fallback;
see [refreshing its source pins](recipes.md#tex-live).

Build output goes directly to `out/logs/build.log`, then is printed after the
command finishes, including on failure. Piping native Windows build processes
through Git Bash's `tee` reproduced the Meson hangs seen in GTK4 and PostgreSQL;
regular-file logging completed the same 1,500-file installation.

Kanboard's replacement v1.2.57 archive was compared with the original
hash-verified archive. Only `app/constants.php` changed: Git's exported ref
decoration dropped `HEAD -> main`, while the release commit and application
files remained the same. The recipe pins the reviewed replacement SHA-256.

### BusyBox source seed

The pinned BusyBox binary is kept as a source artifact next to
`ghcr.io/quasidock/quasidock/busybox:seed-SHA256`, where `SHA256` is the full source hash in
`recipes/busybox.json`. This tag is separate from version, `latest`, and catalog
tags. Neither the recipe nor its hash needs to change when upstream returns 403.

To seed the local default store from an existing binary, checking its bytes
against the recipe first:

```powershell
go run ./cmd/quasidock-publish seed-busybox ./busybox.exe
```

With no file argument, the same command gets only that binary from GHCR's source
artifact. It verifies SHA-256 before caching it, repairs a damaged cached seed,
and never tries the upstream URL. `QUASIDOCK_HOME` selects another store. To share
its verified downloads with suites that create their own stores, set
`SEED_DOWNLOADS` to that store's `cache\downloads` directory.

To publish or replace the seed with registry write credentials configured:

```powershell
go run ./scripts/build.go --strip
go run ./cmd/quasidock-publish seed-busybox ./busybox.exe
out/bin/quasidock.exe build recipes/busybox.json
$busyboxRecipe = Get-Content recipes/busybox.json -Raw | ConvertFrom-Json
$seedImage = 'ghcr.io/quasidock/quasidock/busybox:seed-' + $busyboxRecipe.stages[0].sources[0].sha256
out/bin/quasidock.exe tag busybox $seedImage
out/bin/quasidock.exe push --sources $seedImage
```

The seed tag supplies build inputs, authenticated by the recipe's fixed SHA-256.
The normal Publish images workflow signs the built application candidates before
promotion. Repository CI uses its package token just for seeding; image
verification in Reproduce still pulls anonymously. Make the BusyBox package
public to support anonymous local seeds and forked pull requests, and connect it
to `quasidock/quasidock` with Actions write access for subsequent publication.

### Checks

```powershell
go run ./cmd/quasidock-publish generate --check
go test -count=1 ./...
```

After changing recipe dependencies or the allowlist, regenerate the checked-in
workflow with `go run ./cmd/quasidock-publish generate`. Tests reject a stale graph,
dependency cycles, conflicting pins, and dependencies outside the allowlist.

The existing `scripts/publish.sh` still supports local publishing. Its
`--stage ci-RUN_ID APP-ID` operation stages an already-built allowlisted image without
moving release tags. `scripts/sign-images.sh --images IMAGE...` signs exact
candidate references. `quasidock-publish` is a repository tool, not a shipped
application command.

`push --sources` stores hash-checked downloaded source files and vendor archives.
Local source files and directory snapshots remain in source control. When an
upstream URL fails or serves different bytes, or vendor commands fail, the builder
can recover the requested archive from the image version's source artifact. The
requested SHA-256 remains mandatory; registry contents never override it.
For URL inputs, recipes may additionally name `source_mirrors`: fully qualified,
digest-pinned images checked after the image's own source artifact. This allows
bootstrap to recover exact inputs from an older namespace without pulling its
executable layers or changing image trust settings.

An explicit cached image selection or fully qualified reference survives the new
default registry. Only the configured Quasidock key is bundled, scoped to
`ghcr.io/quasidock/quasidock`. Upstream keys are no longer bundled; add any keys needed for
other registries explicitly with `quasidock trust add`. Existing stores and
containers require no migration.
