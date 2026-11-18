# Lazy running with the Cloud Files API

## Current status

In Quasidock 0.8.0, `quasidock pull --lazy` is implemented in two Go packages:

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

## Not tested

At the time of the 2026-11-04 experiment, the following were not tested:

- **Reboots**: whether a registration survives one (lazy-sshfs found it
  survives a crash), and whether the image section's cost does.
- **Hydration policies** other than Progressive (`CF_HYDRATION_POLICY_PROGRESSIVE`).
- **Real network failures**: a failed Range request takes the same path as the
  failed chunks above, but none was provoked.
- **Other antivirus**, and Defender with exclusions: we couldn't change those
  without admin. Other machines' first-open costs may differ a lot.
