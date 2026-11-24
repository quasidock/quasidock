# How it works

Quasidock builds and runs Windows CLI and GUI applications as standard OCI
images stored in a per-user store without administrator rights. Desktop
integration (command shims, file associations, and URL schemes) and
self-update/rollback were removed in [Quasidock 0.2](iteration-2.md); recipe
`--file`, config overrides, extensions, and user-folder junctions were removed
in [Quasidock 0.3](iteration-3.md).

## Images

Each image is an OCI manifest (`application/vnd.oci.image.manifest.v1+json`)
pointing to a versioned app config blob and `N + 1` filesystem layers ordered as
the image's `from` base runtime layers followed by the app's own layer. v3 adds
image working directories; committed images use v4 and insert one private-data
seed immediately before the app layer
([`oci.go`](../internal/oci/oci.go)).

Layers use deterministic tar, gzip
(`application/vnd.oci.image.layer.v1.tar+gzip`), or eStargz (`tar+gzip` with
the [`estargz.TOCJSONDigestAnnotation`](../internal/estargz/estargz.go)
annotation); plain `application/vnd.oci.image.layer.v1.tar` is also supported
(see [`layer-formats.md`](layer-formats.md)). Standard OCI layouts allow
standard registries (default `ghcr.io/quasidock/quasidock`, overridden by
`QUASIDOCK_REGISTRY`) to store images and mount shared runtime blobs across
repositories without re-uploading.

On pull, [`remote.FetchPrepared`](../internal/remote/remote.go) resolves
multi-platform OCI indexes to `windows/amd64`, verifies any signature before
fetching layers, pulls up to 4 missing blobs in parallel, and can assemble
eStargz blobs by copying unchanged chunks from layers already in the store when
at least 10% (`MinReuseTenths = 1`) of bytes match. Pulls are non-interactive
(`--yes` is accepted only as a compatibility no-op).

The local catalog in `images\index.json` lets multiple versions and tags of an
app coexist ([`images.go`](../internal/store/images.go)):

- `Images` indexes installed manifests by `sha256:...` manifest digest (image
  ID).
- `Names` maps normalized `<host>/<repository>[:tag|@digest]` names to
  manifest digests.
- `Defaults` maps lowercased bare app IDs to the most recently pulled, built, or named committed
  manifest digest.

Legacy `refs\<id>.json` metadata from older stores is migrated once into
`images\index.json` when the store opens.

## Builds

Builds are driven by [`internal/builder`](../internal/builder/builder.go) from
recipes ([`recipes.md`](recipes.md)) along two paths:

- **Assembly-only recipes** (stages declare `sources` without `build`
  commands) do not unpack files to disk. [`Sources`](../internal/builder/sources.go)
  indexes local files, `.zip` archives, and `.tar` streams (inflating `.tar.gz`
  and `.tar.xz` once into `cache\downloads\<sha256>.tar`) into an in-memory
  [`tree.Tree`](../internal/tree/tree.go) and writes the deterministic layer
  directly through [`format.WriteLayer`](../internal/format/format.go), avoiding
  Windows Defender scans of temporary unpacked files.
- **Recipes with `build` commands** create `tmp\quasidock-build-*` and map it
  to `B:` with `DefineDosDevice` under the `Local\quasidock-build-drive` mutex
  ([`drive.go`](../internal/builder/drive.go)), failing if a non-quasidock
  mapping already uses `B:`. Inside `B:\`: `B:\prefix` collects the app layer
  tree, `B:\src\<stage>` holds stage sources and `.zig-cache`, `B:\cache\<tool>`
  maps persistent `cache\tools\<id>` directories, `B:\home` provides the build
  user profile, and `B:\data` holds scratch files.

Every build step runs with `PREFIX=B:\prefix`, `SOURCE_DATE_EPOCH=315532800`,
`QUASIDOCK_BUILD=1`, `ZIG_PROGRESS=off`,
`ZIG_LOCAL_CACHE_DIR=B:\src\<stage>\.zig-cache`, plus `PKG_CONFIG_PATH` and
`CMAKE_PREFIX_PATH` populated from `B:\prefix` and dependency layers. Offline
`build` commands receive `HTTP_PROXY` and `HTTPS_PROXY` pointed at
`http://127.0.0.1:9` unless `stage.network` is enabled. Host toolchains
currently support `"host": ["msvc"]` via `vswhere.exe` and `vcvars64.bat`
([`msvc.go`](../internal/builder/msvc.go)).

Fetched source files cache by SHA-256 in `cache\downloads\<hex>`, falling back
to vendor source layers uploaded via `quasidock push --sources` if an upstream
URL fails or drifts ([`fetch.go`](../internal/builder/fetch.go)). Complete builds
record their manifest digest in `cache\builds\<inputsHex>` (reused unless
`--rebuild`), and project builds retain incremental work under
`cache\work\<16hex>`.

## Store

The store defaults to `%LOCALAPPDATA%\quasidock`, overridden by `QUASIDOCK_HOME`.
Its directories match `subdirs` in [`store.go`](../internal/store/store.go),
alongside runtime, lock, and trust files:

```text
blobs\sha256\<hex>            manifests, configs, layer blobs
images\index.json             image catalog (index.lock, use-<hex>.lock)
containers\<id>\              state.json, container.log, run.lock, data\
  data\home\                  USERPROFILE, AppData\Roaming, AppData\Local, Temp
  data\registry\              per-container HKCU hive (user.dat)
volumes\<name>\               named persistent volumes (volume.json, data\)
refs\<id>.json                legacy per-app refs (imported once into images\)
deploy\<hex>\                 unpacked layers, read-only ACL (deploy\<hex>.lock)
lazy\<hex>\                   sparse blob, QDBLOB01 bitmap, helper state and log
data\<id>\                    legacy app data (removed only by prune --data)
grants\<id>.txt               persisted host grants (.cwd, .programs, .txt.lock)
ports\<id>.txt                persistent internal port allocations for services
relay\                        quasidock-relay-<8hex>.exe loopback relay binary
host\                         quasidock-host.exe wrapper binary
certs\roots-<hex>.pem         exported Windows root CA PEM bundle
cache\downloads\              fetched sources and inflated .tar archives
cache\images\                 build dependency image markers
cache\builds\                 manifest digests keyed by build input hash
cache\tools\                  persistent SDK tool caches (B:\cache\<tool>)
cache\work\                   incremental stage directories for project builds
registry-locks\<sha256>.lock  locks guarding container registry directories
tmp\                          staging, B: build roots, deferred trash-* deletes
trust.json                    user-added registry signing keys
lock, grants.lock             Win32 LockFileEx coordination lock files
```

Concurrent `quasidock` processes synchronize via Win32 `LockFileEx` locks (`lock`
shared for normal commands, exclusive for `prune` and `image rm`; plus index,
`use-<hex>`, `run.lock`, `deploy\<hex>.lock`, `grants.lock`, and registry
locks). Unpacked files in `deploy\<hex>\` carry a `DENY` `FILE_MODIFY` ACL entry
for the current user (`acl.Protect`) so containers cannot alter shared layers.

## Runs and containers

`quasidock run` ([`run.go`](../internal/run/run.go)) takes a shared store lock,
resolves the image, calls `lazy.EnsureServed` for any streaming layers, acquires
shared image (`use-<hex>.lock`) and deployment (`deploy\<hex>.lock`) leases so
`prune` cannot delete active files, deploys missing layers to `deploy\<hex>\`,
and creates a container record in `containers\<id>\state.json` (a 64-hex ID with
either `--name` or a generated `<adjective>_<noun>` name).

```powershell
# Run in foreground and remove container data on exit
quasidock run --rm busybox echo hello

# Run detached and inspect logs
$cid = quasidock run -d --name worker my-app
quasidock logs $cid
quasidock stop -t 5 $cid
quasidock rm $cid
```

- **Foreground vs detached**: foreground runs call `executeContainer` in place
  (keeping stdin open with `-i`, otherwise attaching `\\.\NUL`). `-d` /
  `--detach` launches `quasidock container-supervisor <store> <containerID> <runID>`
  with a hidden console, waits up to 30 s on an event scoped to that launch
  attempt, and prints the 64-hex container ID. A startup timeout terminates the
  supervisor using its owned process handle; closing its Job Object kills the
  application tree. Standard output and error drain concurrently into
  `containers\<id>\container.log`. Background `-i` keeps a supervisor-owned stdin
  pipe open for later attachment, rather than using NUL.
- **Starting and restarting**: saved `RunSpec` inputs and the pinned image digest
  recreate the command without allocating another container or resolving a newer
  tag. Logs append across launches. Operation locks in `containers\operations\`
  serialize commands and removal; pruning skips active operations. Restart holds
  this reservation across shutdown and launch. Exit cleanup checks the run ID
  under the same lock before automatic removal, so the outgoing supervisor cannot
  remove a replacement run.
- **Commit**: the shared store lock protects inherited layers from GC, and the
  container operation lock reserves the source during capture. Running owners
  advertise `State.CommitProtocol` and serve capture leases over their existing
  current-user-only exec pipe. A lease freezes the job, serializes control and
  exec admission, and restores the previous pause state on release/disconnect.
  Private files and declared HKCU keys are staged while frozen; compression and
  catalog registration follow after resuming. A v4 data seed initializes new
  container data before its record is visible; starts never copy it again.
- **Attachment**: background supervisors serve a separate current-user-only local
  named pipe scoped to container ID and RunID. `State.AttachProtocol` advertises
  support. Subscribers receive live stdout/stderr and the primary exit status;
  historical output remains in `logs`. Each subscriber has a bounded 1 MiB output
  queue and a five-second write deadline, so a stalled viewer cannot block logging.
  Only one session owns stdin. A runtime-owned input pump uses acknowledged chunks
  and survives detachment; EOF closes stdin, while disconnect releases its writer.
  Attached start connects and subscribes before child launch, then releases the
  operation lock at startup acknowledgement. Ctrl+C forwards targeted TERM unless
  disabled; interactive console clients interpret detach keys and restore console
  modes on exit. Completion drains output and sends the final status before
  automatic removal. Restart ends the old session instead of switching its RunID.
- **Stopping, killing, and `--rm`**: launch-scoped control events and atomic
  request/result files acknowledge signals. The supervisor sends targeted
  `CTRL_BREAK_EVENT` for TERM or calls `TerminateJobObject` for KILL. TERM does
  not escalate; `stop` and `restart` escalate after their caller-selected timeout
  (default 10 s). Foreground TERM is rejected to retain native console behavior.
  Listing,
  inspecting, or pruning reconciles dead `"running"` containers whose exclusive
  `run.lock` was released (marking them `"exited"` with code `137` if none was
  recorded, or deleting them if `--rm`). `--rm` containers delete
  `containers\<id>\` and temporary registry state on exit.
- **Pause and unpause**: the same launch-scoped control channel carries pause
  actions, advertised by `State.ControlProtocol`. The supervisor freezes or thaws
  the container's Job Object with `SetInformationJobObject`, using the private
  `JobObjectFreezeInformation` class (18). The job contains the main process,
  descendants, relay, and nested exec jobs; its supervisor and stream pumps stay
  outside it. Runtime locking serializes changes with exec admission and TERM
  delivery, including TERM forwarded by existing attachments. Acknowledgement
  follows the Windows operation and persisted state update. Failed persistence
  rolls back the Windows change; failed rollback terminates the job.
  Paused state keeps `Running=true`, sets `Paused=true` and `Status="paused"`, and
  clears on exit or crash reconciliation. Pausing retains all resource locks.
  New exec and attach sessions are rejected while existing sessions survive.
  Graceful shutdown resumes first; KILL and forced removal terminate frozen jobs
  directly. Foreground TERM is still rejected without changing pause state.
- **Exec**: foreground owners and detached supervisors serve a local named pipe
  scoped to container ID and RunID ([`exec.go`](../internal/run/exec.go)). Its ACL permits only
  the current user, and remote clients are rejected. Exec commands reuse the
  owner's startup environment, working directory, sandbox token/capabilities,
  mounts, registry, relays, and write watcher. Each command has a child Job Object
  nested below the container job, allowing cancellation of one exec tree without
  ending the main process. Framed streams carry stdout/stderr, exit status, and
  acknowledged stdin chunks so blocked input does not prevent cancellation.
  Detached exec discards streams; foreground exec output goes only to its caller.
  Shutdown closes launch admission and joins exec workers before restoring
  registry, mount, and grant state. Exec does not allocate another container or
  change its saved command; `State.ExecProtocol` identifies capable live owners.
- **Environment and registry**: [`internal/environment`](../internal/environment/environment.go)
  orders `PATH` as app `path` dirs, `from` runtime dirs,
  `SystemRoot\System32;SystemRoot;SystemRoot\System32\Wbem`, and `pathAfter`
  entries; points `USERPROFILE` and `HOME` at `containers\<id>\data\home`; sets
  `APPDATA`, `LOCALAPPDATA`, `TEMP`, and `TMP` inside it; initializes Quick Access
  state; strips inherited `QUASIDOCK_*` variables; and sets `QUASIDOCK_ID` so
  child `quasidock` processes restore the caller's environment. Bare `-e NAME`
  values or absences are snapshotted in `RunSpec.InheritedEnv` for later starts.
  `${data}`,
  `${cache}`, and `${roots}` resolve to `data`, `data\cache`, and
  `certs\roots-<8hex>.pem`. [`internal/hkcu`](../internal/hkcu/hkcu.go) mounts
  `<data>\registry\user.dat` with `RegLoadAppKey` so each container gets its own
  `HKEY_CURRENT_USER` hive.
- **Mounts (`-v` / `--mount`)**: `-v SOURCE:TARGET` and
  `--mount type=volume|bind,source=...,target=...` attach named volumes
  (`volumes\<name>\data`) or local NTFS host directories at `${data}` or
  `${data}/<subpath>` using NTFS directory junctions (`FSCTL_SET_REPARSE_POINT`,
  [`mounts.go`](../internal/mounts/mounts.go)). Read-only mounts and reparse
  points in mount sources or their ancestors are rejected. Empty
  `containers\<id>\data` is labeled or granted *before* junctions are attached
  so container grants never traverse junctions onto host folders
  ([`run/mounts.go`](../internal/run/mounts.go)).
- **Jobs, commands, and relays**: container processes live inside a
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` job object
  ([`process.go`](../internal/process/process.go)). A command matching the app
  ID (or `entrypoint[0]` stem when `entrypoint` has multiple items) replaces
  `cmd` while keeping `entrypoint`; otherwise commands resolve in
  `[deployDir, appPath..., System32]` across `.exe`, `.com`, `.cmd`, and `.bat`
  with safe batch quoting. Services (`serves`/`uses`) forward through
  `relay\quasidock-relay-<8hex>.exe` (see [`networking.md`](networking.md) and
  [`services.go`](../internal/services/services.go)).

## Sandboxes (`--sandbox=soft|low|appcontainer`)

Sandbox selection resolves in order: `--sandbox` CLI flag -> image
`permissions.sandbox` -> `low` ([`run.go`](../internal/run/run.go)). Before
granting or labeling any host directory, `LabelRefusal` rejects drive roots,
`USERPROFILE` or ancestors containing it, and `grants.Own` (the store root,
`quasidock.exe`'s directory, and ancestors or descendants of either).

- **`soft`**: keeps the user's normal (Medium) integrity token while still
  applying the job object, redirected profile and HKCU hive, mounts, and network
  relay. Prints a warning if the entrypoint executable itself carries a Low
  integrity label.
- **`low`** (default): duplicates the user token at integrity `S-1-16-4096`
  (`IntegrityLow` with `SYSTEM_MANDATORY_LABEL_NO_WRITE_UP`) and labels
  container data and writable host grants Low. Because executables created by a
  Low-integrity process would launch at Medium integrity if opened outside the
  sandbox unless marked Low, [`internal/written`](../internal/written/written.go)
  watches writable container and mount directories via IOCP
  `ReadDirectoryChangesW` (falling back to a bounded 50,000-entry walk on buffer
  overflow) and records `.exe`, `.com`, and `.scr` files with `<= Low` effective
  label in `grants\<id>.programs`. Subsequent non-`soft` runs call
  `written.Relabel` to enforce `IntegrityLow` on those executables.
- **`appcontainer`**: creates or reuses profile `quasidock.<id>`
  ([`appcontainer.go`](../internal/appcontainer/appcontainer.go)) and launches
  with `PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES` containing a per-app
  derived capability SID (`DeriveCapabilitySidsFromName`, allowing clean ACL
  revocation across reinstalls), `S-1-15-3-1` (`InternetClient`) and
  `S-1-15-3-3` (`PrivateNetworkClientServer`) when networking is enabled, and a
  loopback exemption unless `QUASIDOCK_LOOPBACK_EXEMPTION=0`.

AppContainer ACLs and Windows behavior carry tradeoffs documented in
[`findings.md`](findings.md): labeling or granting 20,000 files takes roughly 2 s
(2.2 s low / 2.4 s AppContainer);
Windows 11 26300.9550 fixed path resolution for AppContainers so Python, Zig,
`git`, and Node work, while Windows Server 2025 still blocks opening
`NUL` inside AppContainers.

## Lazy pulls

`quasidock pull --lazy <image>` (or `quasidock image pull --lazy <image>`)
accepts eligible eStargz layers (`>= 32 MiB`, overrideable via
`QUASIDOCK_TEST_LAZY_MIN`) that pass `CheckDeployable` and have digests on every
chunk ([`lazy.go`](../internal/lazy/lazy.go)).

1. `lazy.Prepare` fetches the eStargz tail (`TailStart` to EOF), verifies the TOC
   digest, writes `lazy\<hex>\tail`, `layer.json`, and `state.json`, takes a
   shared `lazy\<hex>.pull.lock`, copies `quasidock.exe` to
   `lazy\quasidock-<8hex>.exe`, spawns detached
   `quasidock lazy-helper <store> <hex>`, and waits up to 120 s for
   `Local\quasidock-<storeHash>-<hex[:16]>-ready`. Subsequent `run` or `build`
   invocations call `lazy.EnsureServed` to restart the helper if needed.
2. [`internal/lazy/helper.go`](../internal/lazy/helper.go) takes exclusive
   `lazy\<hex>.helper.lock` and opens [`BlobReader`](../internal/lazy/blobreader.go)
   (3 workers) over `lazy\<hex>\blob.sparse` (NTFS sparse file pre-populated
   with the tail) and `lazy\<hex>\have` (24-byte `"QDBLOB01"` header storing
   `BlockSize = 1 MiB` and `Size`, plus a 1-bit-per-MiB block bitmap).
3. The helper registers `deploy\<hex>\` as a Cloud Files sync root
   (`CF_HYDRATION_POLICY_PROGRESSIVE`, `CF_POPULATION_POLICY_ALWAYS_FULL`),
   creates placeholders via `cloudfiles.CreatePlaceholders`, and signals ready.
4. `BlobReader` supports 4 MiB priority `DemandPiece` reads with a 1000 ms quiet
   window before 8 MiB `BackgroundPiece` sequential fetches. In Quasidock 0.8.0,
   [`internal/cloudfiles`](../internal/cloudfiles/cloudfiles.go) binds
   `CfConnectSyncRoot`, `Hydrate`, `Transfer`, and `ReportProgress`, but
   `RunHelper` does not yet wire read callbacks or prefetch hydration: once
   `pull` drops `lazy\<hex>.pull.lock`, `RunHelper` runs `DownloadAll()`
   directly, refreshing `state.json` every 500 ms (and honoring the `stop` event
   with a 15 s grace window before `TerminateProcess`).
5. When `DownloadAll()` finishes, `helper.finish()` SHA-256 verifies
   `blob.sparse`, moves it to `blobs\sha256\<hex>`, extracts into
   `tmp\lazy-unpack*`, waits (200 ms poll) until `!DeploymentInUse`, unregisters
   the sync root, removes placeholders, renames `tmp\lazy-unpack*` into
   `deploy\<hex>\`, and deletes `lazy\<hex>`. Replacing placeholders with plain
   files avoids Windows's per-process image-section penalty on hydrated DLLs.

## Signatures

Image verification uses `cosign`'s classic layout
([`signature.go`](../internal/signature/signature.go),
[`trust.go`](../internal/trust/trust.go)): a manifest tagged `sha256-<hex>.sig`
holding a `application/vnd.dev.cosign.simplesigning.v1+json` payload (up to 64
KiB, duplicate JSON keys rejected) with `docker-manifest-digest` and
case-insensitive `docker-reference`, plus a base64 DER ECDSA P-256 signature over
SHA-256 in annotation `dev.cosignproject.cosign/signature`.

The embedded public key [`internal/trust/quasidock.pub`](../internal/trust/quasidock.pub)
covers `ghcr.io/quasidock/quasidock` (`QuasidockPrefix`), matched by whole path components
(`UnderPrefix`); user-added keys are stored in `<store>\trust.json` via
`quasidock trust add|rm` (see [`publishing.md`](publishing.md)).

```bash
quasidock signature payload ghcr.io/me/app:1.0 > payload.json
cosign sign-blob --yes --key cosign.key payload.json > payload.sig
quasidock signature push ghcr.io/me/app:1.0 payload.json payload.sig
quasidock signature check ghcr.io/me/app:1.0
```

During `pull`, [`remote.CheckSignature`](../internal/remote/remote.go) runs
before layers download: a bad signature aborts the pull without installing, an
unsigned image logs a warning and continues, and registries without a configured
key note that signatures were not checked. `quasidock signature check` succeeds
only when verified by a trusted key. Release archives (`SHA256SUMS` and
`SHA256SUMS.sig`) are signed externally with `cosign` in
[`scripts/sign-release.sh`](../scripts/sign-release.sh) after reproducible
rebuild checks; there is no `upgrade` command.

## How long grants last

Host path grants (directories or files passed on the command line or `--grant`,
plus granted working directories) are persisted per app in `grants\<id>.txt`,
`grants\<id>.cwd`, and `grants\<id>.programs`
([`grants.go`](../internal/grants/grants.go)). Loopback grants expire when a run
ends.

Mount grants (`-v` / `--mount`) are tracked across stores by process-backed
ownership in the Windows Registry at `HKCU\Software\Quasidock\Grants\Mounts`
(value `"Sources"`, root key overridden by `QUASIDOCK_GRANTS_KEY`), rather than
a file in `grants\` ([`mounts.go`](../internal/grants/mounts.go)). Serialized by
`Local\quasidock-mount-grants`, each mount source records live
`containers\<id>\run.lock` owners alongside baseline ACL and integrity state so
host permissions are restored only once the last live container using that path
exits.

- `quasidock grants [APP-ID]` lists recorded host grants in `<store>\grants\`
  and active container mount storage for that store.
- `quasidock grants --take-back PATH` unlabels an absolute path immediately if
  no running container protects it.
- `quasidock image rm` runs `grants.UndoAll` when the last cached image for an
  `id` is removed (unlabeling `low` paths and revoking AppContainer capability
  SIDs unless protected by a live mount), deletes `grants\<id>.*`, deletes the
  `quasidock.<id>` AppContainer profile, removes `ports\<id>.txt`, and settles
  leftover HKCU state.
- `quasidock repair` recovers after killed or crashed containers: under a shared
  store lock it settles leftover HKCU entries, runs `grants.RepairMounts` (pruning
  dead `run.lock` owners from `HKCU\...\Mounts`, restoring unowned mount
  ACLs/labels, and removing the empty key), and runs `grants.SettleAll` and
  `grants.SettleOrphans` (where [`internal/ledger`](../internal/ledger/ledger.go)
  defines cross-store `HKCU\Software\Quasidock\Grants` records, though in 0.8.0
  `grants.Settle` only prunes loopback and missing paths from `grants\<id>.txt`
  and `SettleOrphans` is a stub returning `0`).
