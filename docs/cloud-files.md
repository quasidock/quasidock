# Experiment: lazy running with the Cloud Files API

This is a historical experiment from Zigsaw (Quasidock's Zig predecessor), run
on **2026-11-04** on Zigsaw's `experiment/cfapi` branch. No Zigsaw code changed:
the provider was a standalone Zig test program (`tests/cfprobe.zig` on that
branch), which created a placeholder folder where a deployment belonged so an
unmodified `zigsaw run` used it like any other deployment.

Zigsaw [iteration 77](../legacy/iteration-77.md) (and [iteration
78](../legacy/iteration-78.md) for parallel and prioritized prefetching) built
the bridge this experiment recommended as `pull --lazy`, after the same probe
passed on GitHub's Windows Server 2025. That feature is now implemented in Go
for `quasidock pull --lazy` under
[`internal/cloudfiles/cloudfiles.go`](../internal/cloudfiles/cloudfiles.go) and
[`internal/lazy/lazy.go`](../internal/lazy/lazy.go) (see
[how-it-works.md#lazy-pulls](how-it-works.md#lazy-pulls) and
[Cloud Files in Quasidock today](#cloud-files-in-quasidock-today)).

The question the experiment set out to answer: can an app run from an eStargz
layer that hasn't been downloaded, with Windows asking for each file as
something first opens it, without admin rights and under the sandbox? [Layer
formats](layer-formats.md) had left this open because the Cloud Files API was
untested with AppContainer and low IL.

In the 2026-11-04 Zigsaw runs, the images were `llvm-mingw` (110 MB blob,
5,381 files, 404 MB) and Qt's SDK (own layer 96 MB, 10,195 files, 318 MB), both
already on eStargz. The machine ran Windows 11 Home 10.0.26300 with Microsoft
Defender's real-time protection on and no exclusions, on an NVMe SSD (Ryzen 7
5700U, 15 GB RAM), on a link that downloaded from `ghcr.io`'s storage at about
15 MB/s. The lazy runs over the network read the blob from `ghcr.io`'s storage
by HTTP Range requests, with the local copy moved away.

## Cloud Files in Quasidock today

In Quasidock 0.8.0, `quasidock pull --lazy` uses two Go packages ported from
Zigsaw's implementation:

- **[`internal/cloudfiles/cloudfiles.go`](../internal/cloudfiles/cloudfiles.go)**
  loads `cldapi.dll` dynamically (`LoadLibrary` / `GetProcAddress`) and resolves
  `CfRegisterSyncRoot`, `CfUnregisterSyncRoot`, `CfConnectSyncRoot`,
  `CfDisconnectSyncRoot`, `CfExecute`, `CfCreatePlaceholders`,
  `CfReportProviderProgress`, `CfGetSyncRootInfoByPath`, and
  `CfHydratePlaceholder`. It exports:
  - `Register` (`ProviderName` `"quasidock"`, `ProviderID`,
    `CF_HYDRATION_POLICY_PROGRESSIVE`, `CF_POPULATION_POLICY_ALWAYS_FULL`,
    `CF_REGISTER_FLAG_DISABLE_ON_DEMAND_POPULATION_ON_ROOT |
    CF_REGISTER_FLAG_MARK_IN_SYNC_ON_ROOT`, and optional
    `CF_REGISTER_FLAG_UPDATE`);
  - `Unregister` (ignoring error codes `2`, `3`, and `405`) and `IsSyncRoot`;
  - `CreatePlaceholders` (with `CF_PLACEHOLDER_CREATE_FLAG_MARK_IN_SYNC`,
    `CF_PLACEHOLDER_CREATE_FLAG_DISABLE_ON_DEMAND_POPULATION` on directories,
    and 4-byte `uint32` identities);
  - `Hydrate` (opens a handle with `FILE_READ_ATTRIBUTES` and calls
    `CfHydratePlaceholder` for the full file), `Connection.Disconnect`,
    `Transfer` (`CfExecute` with `CF_OPERATION_TYPE_TRANSFER_DATA`),
    `ReportProgress`, `IdentityOf`, and `ProcessImage`;
  - `MeansUnsupported` / `ErrUnsupported`, which map Windows errors `1`, `50`,
    `379`, `381`, and `478` (such as a Dev Drive) so `quasidock pull --lazy`
    falls back to downloading the whole layer.
  - Note: while `cfConnectSyncRoot` is resolved and callback/transfer types
    (`CallbackInfo`, `FetchDataParams`, `CancelParams`, `CallbackRegistration`,
    `Transfer`, `ReportProgress`) are defined in `cloudfiles.go`, no `Connect`
    wrapper is exposed or wired yet in the Go port.
- **[`internal/lazy/`](../internal/lazy/lazy.go)**
  ([`lazy.go`](../internal/lazy/lazy.go),
  [`helper.go`](../internal/lazy/helper.go),
  [`blobreader.go`](../internal/lazy/blobreader.go)):
  - `quasidock pull --lazy` ([`internal/cli/images.go`](../internal/cli/images.go))
    checks `lazy.Eligible` (indexed eStargz layers of at least `DefaultMinSize`
    = 32 MiB, overridable with `QUASIDOCK_TEST_LAZY_MIN`) and calls
    `lazy.Prepare`, which fetches the eStargz TOC via
    [`internal/remote/remote.go`](../internal/remote/remote.go), checks
    deployability and chunk digests, writes `lazy\<hex>\tail`, `layer.json`,
    and `state.json`, holds `lazy\<hex>.pull.lock` shared, and launches a
    detached helper copy (`lazy\quasidock-<hash>.exe lazy-helper <store>
    <hex>`) with `CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS`.
  - `RunHelper` acquires `lazy\<hex>.helper.lock`, attaches sparse file
    `lazy\<hex>\blob.sparse` and 1 MiB bitmap `lazy\<hex>\have` (magic
    `"QDBLOB01"`), registers `deploy\<hex>\` via `cloudfiles.Register`, creates
    placeholders via `cloudfiles.CreatePlaceholders`, signals the `ready` event,
    waits for `pull.lock` to clear, and runs `BlobReader.DownloadAll` (1 MiB
    blocks, 4 MiB demand / 8 MiB background pieces, 3 concurrent registry
    clients by default). Once verified against SHA-256, `finish` moves the
    blob into `blobs\sha256\`, unpacks into `tmp\`, waits for `DeploymentInUse`
    locks on `deploy\<hex>\` to clear, calls `cloudfiles.Unregister`, deletes
    `deploy\<hex>\`, renames the unpacked tree into place, and deletes
    `lazy\<hex>\`.
  - Because `CfConnectSyncRoot` / `FETCH_DATA` callback serving and `Hydrate`
    prefetching are not yet wired into `RunHelper`, placeholder reads before
    the background download swaps in the unpacked tree currently have no active
    callback provider.
  - `quasidock run` ([`internal/run/run.go`](../internal/run/run.go)) and
    [`internal/builder/deps.go`](../internal/builder/deps.go) call
    `lazy.EnsureServed` before locking layers, and `run` surfaces new
    `fetch failed:` lines from `lazy\<hex>\log` on non-zero exits via
    `lazy.FailedFetches`. `quasidock inspect` populates `Lazy` via
    `lazy.Describe`, `quasidock push` refuses incomplete lazy layers, and
    `quasidock prune` ([`internal/lifecycle/prune.go`](../internal/lifecycle/prune.go))
    cleans orphaned `lazy\` state via `lazy.Prune`. (`lazy.Stop` exists in
    `lazy.go`, though `quasidock rmi` does not currently call it.)

## Outcome

From the 2026-11-04 Zigsaw experiment (`tests/cfprobe.zig`):

- **It works, without admin, in every sandbox.** A non-elevated process can
  register a folder, connect, and make placeholders. Under low integrity and
  under AppContainer, a process that reads, maps or loads a dehydrated file, or
  starts a dehydrated program, gets it. The deny entry and capability grants on
  the deployment (`deploy\<hex>\`) don't get in the way.
- **It fit Zigsaw's store unchanged.** `zigsaw run`, Zigsaw's `rm`, and `prune`
  worked on such a folder with no change (corresponding to `quasidock run`,
  `quasidock rmi`, and `quasidock prune`). Roots under `%LOCALAPPDATA%` work, and
  each cost about 38 ms to register, process start included.
  `CfRegisterSyncRoot` adds nothing to the registry keys that list sync roots or
  Explorer's navigation pane.
- **Time to first use fell 1.7 to 8 times, by how little of the image is used**
  (measured in the 2026-11-04 Zigsaw experiment):

  | from nothing | full pull | lazily | blob read |
  |---|---|---|---|
  | Qt SDK's own layer, `moc --version` | ~24 s (that layer only) | 3.0 s | 8 of 92 MB, with `rcc` and `qmake` |
  | llvm-mingw, `clang --version` | 17 s | 6.4 s | 51 of 105 MB |
  | llvm-mingw, `clang hello.c` | 17 s | 7.5 s | 64 of 105 MB |
  | llvm-mingw, `clang++` with `<iostream>` | 18 s | 10.9 s | 70 of 105 MB |
  | the same, replaying a recorded file list | 18 s | 7.6 s | 70 of 105 MB |

  Lazily means blocks of 1 MiB. `llvm-mingw` is close to the worst case: two
  DLLs are 45% of its blob, and almost every command loads them. (Later end-to-end
  measurements on real multi-layer Zigsaw images are in [Zigsaw iteration
  77](../legacy/iteration-77.md) and [Zigsaw iteration
  78](../legacy/iteration-78.md).)
- **Fetching per file loses to a full pull.** With one request per file, that
  `clang++` took 27 s (though it ran first, on the coldest CDN). A scattered
  4 KiB range from the registry's storage took 0.65 to 1.3 s when it missed the
  CDN's cache, about as long as 16 MiB (1.06 s). Blocks of 1 MiB make 70
  requests of those 464.
- **Warm runs cost more for big images.** Creating an image section for a
  hydrated 77 MB DLL takes 60 ms every time, against 0.5 ms for a plain copy of
  the same bytes. `clang --version` goes from 67 to ~155 ms, unless the file is
  replaced. Small programs aren't affected. We didn't find the cause; see
  [The image section](#the-image-section).
- **First opens are deferred, not avoided.** Each file's first open costs
  ~7.5 ms more, one after another, which is 34 to 51 s to read all of
  `llvm-mingw` for the first time, against 0.4 s after an unpack. It looks like
  Defender scanning a file it hasn't seen, which an unpack pays for when
  writing, but we couldn't prove that without admin. Hydrating itself is cheap:
  from a local blob, 8 threads hydrated all 5,381 files in 1.4 to 1.9 s,
  against 5.8 s to unpack.
- **One root has one provider**, and it has to stay. A second connection is
  refused (`ERROR_CLOUD_FILE_ALREADY_CONNECTED`). Once the provider is gone,
  dehydrated files fail, and a program or DLL fails with **`BAD_EXE_FORMAT`**
  (193), which says nothing of the cause.
- **NTFS only, here.** A Dev Drive refuses it: error 478, "the file system
  minifilter cannot attach to the developer volume". Microsoft documents a way
  for an admin to allow filters on a Dev Drive; we didn't try that, or ReFS
  outside a Dev Drive. (`internal/cloudfiles` maps error 478 to
  `ErrUnsupported` so `quasidock pull --lazy` falls back to a full download.)

**Recommendation:** lazy running is worth building as a **bridge**, not as a way
of keeping apps. The first run starts from placeholders while the blob downloads
in the background; when the download is complete and checked, the helper unpacks
it and removes the sync root. That keeps warm runs as fast as plain deployments,
needs a provider only while a root is in use, and preserves the invariant that a
blob in the store is complete and checked against its digest once the bridge is
over. See [What a bridge would need](#what-a-bridge-would-need). This reversed an
earlier Zigsaw decision of 2026-10-31 ("lazy running is out of scope for now")
and adapted a principle of [Layer formats](layer-formats.md) ("no filesystem
driver", though no driver is installed here: `cldflt` comes with Windows).
Zigsaw [iteration 77](../legacy/iteration-77.md) implemented this bridge as
`pull --lazy`, and Quasidock carries that design forward.

## Method

In the 2026-11-04 Zigsaw experiment, `zigsaw-cfprobe mount <root> <blob or URL>
-- <command>` opened the blob's TOC through Zigsaw's `stargazer` library (now
replaced in Quasidock by [`internal/estargz`](../internal/estargz/estargz.go)
and [`internal/format`](../internal/format/format.go)), registered the folder
with `CfRegisterSyncRoot` (Progressive hydration, population `ALWAYS_FULL`, so
that no directory ever needs a callback), made a placeholder of each directory
and file with `CfCreatePlaceholders`, directory by directory, and ran the
command. On `FETCH_DATA` it read and inflated the chunks covering the range,
checked each against its digest from the TOC, and handed them over with
`CfExecute`. It logged each fetch and the process that caused it. `cldapi.dll`
was loaded at run time, as Zig had no import library for it (Go's
[`internal/cloudfiles/cloudfiles.go`](../internal/cloudfiles/cloudfiles.go)
still loads `cldapi.dll` at runtime via `syscall.LoadLibrary`).

The folder was the layer's `deploy\<hex>\`. Zigsaw's `Store.deploy` (now
`Store.Deploy` in [`internal/store/store.go`](../internal/store/store.go)) found
it there and didn't unpack, then protected it as usual, so what ran was the
real runtime path. `--block-kib` read whole blocks into a cache. A URL read
ranges through WinHTTP, from the storage `ghcr.io` redirects to. `--net-ms`
slowed a local blob down, for the failure tests.

Times came from `bench.exe`, a tool from Zigsaw's `experiment/vhdx` branch, so
that no shell's startup was in them: `exec` started a program and waited for it,
and reported the first run and the median. "From nothing" added the TOC and the
placeholders to the first run.

## Measurements

All measurements below were collected on Zigsaw in the 2026-11-04 experiment.

### Deploying

| llvm-mingw | unpack | placeholders |
|---|---|---|
| making the tree | 5.8 s | 0.55 s |
| protecting it | 0.44 s | not timed |
| reading the TOC, by Range | | 0.3 to 1.6 s |
| walking it | 14 ms | 18 ms |
| `stat` of every entry, with its DOS path | 313 ms | ~375 ms, with nothing fetched |
| reading every file, warm | 334 ms | 465 ms |
| reading every file, first time | 361 ms | 34 to 51 s |

Qt SDK's layer: 13.4 s to unpack and 1.2 s to protect, against 1.35 s for
11,001 placeholders.

A plain open fetches the whole file, as the lazy-sshfs project found of the same
API: a 300 KB file's open took 12 ms and its first read 0.5 ms. So what's lazy is
the file, not the page, which suits eStargz and rules out reading part of a
large DLL. `stat`, walking and `GetFileAttributes` fetch nothing, and
`GetFinalPathNameByHandleW` gives a DOS path, under AppContainer too: the
realpath failures of the VHDX experiment don't arise.

### From nothing, real network

One command each, on a fresh root. These times are the command's alone:

| blocks | `clang --version` | requests | blob read |
|---|---|---|---|
| none | 5.7 s | 37 | 47.7 MB |
| 256 KiB | 7.6 s | 195 | 48.8 MB |
| 1 MiB | 5.6 s | 51 | 51.0 MB |
| 4 MiB | 4.9 s | 14 | 56.0 MB |
| 16 MiB | 4.5 s | 4 | 64.0 MB |

| blocks | `clang hello.c` | requests | blob read |
|---|---|---|---|
| none | 6.9 s | 62 | 50.5 MB |
| 1 MiB | 6.6 s | 64 | 64.0 MB |
| 4 MiB | 8.5 s | 22 | 88.0 MB |

| blocks | `clang++` with `<iostream>` (434 files) | requests | blob read |
|---|---|---|---|
| none | 24.7 s | 464 | 51.9 MB |
| 1 MiB | 10.0 s | 70 | 70.0 MB |
| 4 MiB | 9.9 s | 22 | 88.0 MB |
| 1 MiB, list replayed on 8 threads first | 5.4 s + 1.4 s | 70 | 70.0 MB |
| 1 MiB, `--sandbox=low` | 10.9 s | 70 | 70.0 MB |

Add the TOC (0.3 to 1.6 s) and the placeholders (0.55 s) to each. A real
deployment runs that `clang++` in 0.56 s the first time. Full pulls, measured
last, after these runs: 17 s for `llvm-mingw` (8 s to download, 7 s to unpack)
and 41 s for Qt's SDK; measured first, 20 and 44 s. Each row is one run, on a
CDN that was warming as we went: treat differences of a second or so as noise.

Qt's SDK, its own layer lazily and its other layers as usual, from nothing: TOC
0.37 s, 11,001 placeholders 1.35 s, `moc --version` 1.24 s, then `rcc` 1.04 s
and `qmake` 0.21 s. The three read 8 MB of the blob's 92, in 8 requests.

### Warm

The median of three, with every file they use hydrated. Measured through
`zigsaw run`, except for `busybox`, which was started directly:

| | real deployment | hydrated | |
|---|---|---|---|
| `clang --version` | 67 ms | 151 to 164 ms | loads libLLVM and libclang-cpp |
| `clang -c hello.c` | 87 ms | 174 to 188 ms | |
| `clang hello.c` | 173 ms | 256 ms | |
| `clang++` with `<iostream>` | 496 ms | 654 to 676 ms | |
| `moc --version` (Qt) | 43 ms | 45 ms | small images |
| busybox (719 KB, no DLLs) | 14 ms | 15 ms | no difference |

### The image section

The cost goes with the size of the DLLs a process loads, and is one step of
loading them. For `libLLVM-23.dll`, 77 MB:

| | open | `CreateFileMapping(SEC_IMAGE)` | map |
|---|---|---|---|
| a plain copy | 0.10 ms | 0.5 ms | 0.06 ms |
| as hydrated | 0.09 ms | **60 ms** | 0.07 ms |
| as hydrated, another process holding it mapped | 0.07 ms | 0.7 ms | 0.05 ms |

So Windows seems to throw away the image section of such a file when its last
user closes it, and rebuild it for the next. What we ruled out or found:

| the file | section | so |
|---|---|---|
| placeholder, provider connected | 60 ms | |
| placeholder, no provider | 60 ms | not the callbacks |
| reverted to a plain file (`CfRevertPlaceholder`) and unregistered | 60 ms | not the reparse point or the root |
| that, with one byte rewritten in place and its time set | 60 ms | not a missing write |
| after a non-buffered open | 60 ms | no change, though we can't say it dropped what's in memory |
| **a plain copy renamed over it, inside the root** | **0.5 ms** | **431 ms, once** |

The hydrated file isn't sparse, and has fewer extents than a copy (19 against
40). It loads at the same address with the same private memory, so the process
isn't relocating it. The children's own CPU time triples (user ~25 ms against
~9, kernel ~45 against ~13). Defender's CPU, by its performance counter, didn't
settle anything. Whether the state is on disk or in memory, and so whether a
reboot cures it, is **not known**.

### When things go wrong

| what | what the reader sees |
|---|---|
| no provider, reading | open succeeds, `ReadFile` fails: 362, `PROVIDER_NOT_RUNNING` |
| no provider, mapping | 362 |
| no provider, `LoadLibrary`, `CreateProcess` | **193, `BAD_EXE_FORMAT`** |
| the provider killed under a waiting reader | at once: 404, `PROVIDER_TERMINATED`; no hang; a new provider then serves the file |
| a chunk failing its digest | 383, `VALIDATION_FAILED`; the file stays dehydrated; Windows asks twice |
| a chunk that won't inflate | 389, `UNSUCCESSFUL`; the same |
| a stall of 70 s | Windows cancels at 60 s (`IO_TIMEOUT`); the read asks again, blocks, and succeeds when the data arrives |
| a second provider | connecting fails: 378, `ALREADY_CONNECTED` |
| the provider's run ends while another uses the layer | the other gets 362 on what isn't hydrated yet |

Two more Windows behaviors that shape a design:

- **A provider's own process is different.** With
  `CF_CONNECT_FLAG_BLOCK_SELF_IMPLICIT_HYDRATION`, which lazy-sshfs found it
  needs, it can't start a dehydrated program (395, `ACCESS_DENIED`), though
  `CfHydratePlaceholder` first works (on a handle that only reads attributes, as
  `cloudfiles.Hydrate` does). Zig's directory walker found no files in the tree
  there, as placeholders show as reparse points to that process. And only that
  process may call `CfRevertPlaceholder` (another gets 395).
- **Unregistering deletes dehydrated files and keeps hydrated ones.** Zigsaw's
  `rm` deleted a protected, partly hydrated root with no provider, and Windows
  had nothing to unregister afterwards. (In Quasidock, `quasidock rmi` and
  `quasidock prune` delete unused deployments once container and run locks are
  released, and `lazy.Prune` cleans up leftover `lazy\` state.)

### Sandboxes

On a fresh root each time, with the store's deny entry on it and, under
AppContainer, the app's capability granted on it:

| a confined process... | soft | low | appcontainer |
|---|---|---|---|
| starts a dehydrated program | ok | ok | ok |
| `ReadFile` on a dehydrated file | ok | ok | ok |
| `MapViewOfFile` on one | ok | ok | ok |
| `LoadLibrary` of one | ok | ok | ok |
| lists `D:\_foss\zigsaw\docs\gaps.md` | not run | ok | **denied**, as it should be |

The provider's log named the confined process as the one fetching each file.
That last row shows the AppContainer was confined (reading the Zigsaw repo's
`docs\gaps.md` path used in the 2026-11-04 test), and the low run's token said
`Mandatory Label\Low`. `clang++` itself fails under AppContainer with
`clang-23: Permission denied`, on a real deployment too, so that's the
toolchain's gap and not Cloud Files'; its image asks for `soft`.

## What a bridge would need

The 2026-11-04 experiment proposed the following six-part bridge design (later
implemented in Zigsaw [iteration 77](../legacy/iteration-77.md) and [iteration
78](../legacy/iteration-78.md), and ported to Go in
[`internal/lazy/lazy.go`](../internal/lazy/lazy.go) and
[`internal/cloudfiles/cloudfiles.go`](../internal/cloudfiles/cloudfiles.go)):

1. **Starting**: for a layer the store lacks, with an index, prepare a sync root
   at `deploy\<hex>\` from the TOC, already checked against the manifest, so
   that the path an app sees never changes. Any other layer deploys as usual, as
   does everything on a volume that refuses it (such as a Dev Drive, where
   `CfRegisterSyncRoot` fails at once with error 478 / `ErrUnsupported`).
2. **A process of its own**: one provider per root, which must outlive the run
   that started it if another joins, and which sees the tree differently, all
   point to a small detached helper per layer (`quasidock lazy-helper`), gone
   when the download finishes and no run holds `deploy\<hex>.lock`. That also
   keeps `quasidock run` ordinary.
3. **Reading**: through blocks of 1 MiB, chunks checked against the TOC, by
   `RemoteBlob` / `BlobReader`, which handles registry credentials and
   redirects. Report progress on slow fetches (`CfReportProviderProgress`), to
   stay inside Windows' 60 s timeout. (See [gaps.md](gaps.md) for current
   limits.)
4. **The download**: the whole blob, in the background, checked against its
   SHA-256 digest at the end.
5. **Ending**: with the blob in and no run holding the deployment's lock,
   unpack it into `tmp\`, unregister and delete the placeholder root, and rename
   the new tree into `deploy\<hex>\`.
6. **Saying why**: a failed fetch should reach the user in words, as apps will
   only ever say `BAD_EXE_FORMAT` or similar. The helper logs `fetch failed:`
   lines to `lazy\<hex>\log`, where `quasidock run` looks when a container exits
   with a non-zero status.

And later, if a list proves worth keeping: eStargz's prioritized files are for
just this (`"prefetch"` in recipes). A list recorded earlier, hydrated and
opened on several threads before the run starts, cut that `clang++` from 10.9
to 7.6 s in the experiment, by taking the first opens in parallel.

The alternatives, and why not:

- **Keeping roots for good**, for images used little: saves disk, but keeps the
  provider, the failure modes and the warm cost for big images, and each run
  needs the registry reachable for files it hasn't used yet. Replacing big
  images with plain copies would fix the warm cost alone.
- **Holding sections open in the helper** avoids the 60 ms too, but only while
  it runs.

## Not tested

At the time of the 2026-11-04 experiment, the following were not tested:

- **Reboots**: whether a registration survives one (lazy-sshfs found it
  survives a crash), and whether the image section's cost does.
- **Hydration policies** other than Progressive (`CF_HYDRATION_POLICY_PROGRESSIVE`).
- **Real network failures**: a failed Range request takes the same path as the
  failed chunks above, but none was provoked.
- **Other antivirus**, and Defender with exclusions: we couldn't change those
  without admin. Other machines' first-open costs may differ a lot.
