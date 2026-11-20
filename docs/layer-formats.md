# Layer formats

Quasidock builds, stores, pulls, and inspects OCI image layers as
uncompressed tar, gzip, or eStargz blobs. This document explains how layer
formats work across [`internal/format`](../internal/format),
[`internal/estargz`](../internal/estargz),
[`internal/remote`](../internal/remote), [`internal/peek`](../internal/peek),
and [`internal/lazy`](../internal/lazy), and records the decisions behind the
design.

## What eStargz is

An eStargz blob (from containerd's
[`stargz-snapshotter/estargz`](../go.mod)) is a standard `.tar.gz` stream
whose tar entries are split into individually compressed gzip members rather
than one monolithic deflate stream. Regular files larger than the chunk size
(`estargz.DefaultChunkSize`, 4 MiB) are split into independent compressed chunks.
Appended to the end of the archive are a JSON table of contents
(`estargz.TOCTarName`) and a fixed-size footer (`estargz.FooterSize`). Every
entry in the TOC records its path, tar metadata, uncompressed size, chunk
offset and size, digest (`Digest` / `ChunkDigest`), and compressed byte offsets
(`Offset` and `NextOffset()`) inside the blob.

Because every member is self-contained and the index sits at the tail of the
blob:

- **A plain gzip client sees an ordinary layer.** The blob carries the standard
  OCI gzip layer media type (`oci.MediaTypeLayerTarGzip`), and a multistream
  gzip reader unpacks all concatenated members sequentially.
- **An eStargz client reads the TOC in one HTTP `Range` request.** Fetching
  only the tail (`remote.TailLen`) yields the complete file list, offsets, and
  digests without downloading the rest of the blob.
- **Individual files and ranges can be fetched independently.** Knowing a
  member's `[Offset, NextOffset())` range lets a reader pull a single file,
  assemble a new layer version from unchanged members on disk, or prepare for
  on-demand hydration.
- **Every read is digest-verified.** Entry and chunk digests sit inside the TOC,
  the TOC JSON digest is carried on the layer descriptor in
  `estargz.TOCJSONDigestAnnotation`, and the whole blob is hashed against its
  OCI descriptor `Digest` when committed to the local store.

## Principles

1. **Standard OCI media types and transparent fallback.** Layers only use
   `oci.MediaTypeLayerTar` (`application/vnd.oci.image.tar`) or
   `oci.MediaTypeLayerTarGzip` (`application/vnd.oci.image.layer.v1.tar+gzip`).
   No custom media types are needed, and a non-indexed puller can unpack an
   eStargz blob through multistream gzip.
2. **Capabilities over format names.** Code outside `internal/format` asks
   `format.FormatOfDescriptor(desc)` for a `Format` and gates indexed features
   on `f.Indexed()` rather than switching on recipe metadata or annotations
   directly.
3. **Wrap the reference eStargz library rather than reimplementing it.**
   [`internal/estargz`](../internal/estargz) wraps
   `github.com/containerd/stargz-snapshotter/estargz` (`v0.19.1`), isolating
   its options, landmark constants, and TOC structures from builders, pullers,
   and the store.
4. **Verify digests before trusting anything on disk.** Downloaded and
   delta-reassembled blobs are SHA-256 verified against the manifest
   descriptor before `store.PutBlobFile` accepts them, and
   `format.CheckDeployable` plus `TOCJSONDigestAnnotation` checks run before
   extracting eStargz layers.

## Design

### Formats and capabilities

[`internal/format/format.go`](../internal/format/format.go) models three
concrete layer formats through `Format` and inspects OCI descriptors with
`FormatOfDescriptor(desc)`:

| Format | Media type | Descriptor signal | `Indexed()` | Typical use |
| --- | --- | --- | --- | --- |
| `FormatTar` | `oci.MediaTypeLayerTar` | Media type | `false` | Vendoring (`builder.vendor`) |
| `FormatGzip` | `oci.MediaTypeLayerTarGzip` | No `TOCJSONDigestAnnotation` | `false` | Default recipe layer |
| `FormatEstargz` | `oci.MediaTypeLayerTarGzip` | `estargz.TOCJSONDigestAnnotation` | `true` | `"layer": "estargz"` recipes |

Unrecognized media types (for example `tar+zstd`) return
`(FormatUnknown, false)` from `FormatOfDescriptor`, and `oci.ValidateLayers`
rejects anything other than `MediaTypeLayerTar` and `MediaTypeLayerTarGzip`.
`quasidock image info` prints the `layer` field (such as `eStargz` or `tar`)
whenever a layer's format is not `FormatGzip`.

Quasidock keeps `estargz.DefaultChunkSize = 4 * 1024 * 1024` (4 MiB) so large trees stay near
plain gzip sizes while large files remain split into manageable chunks.

### The writer

[`internal/builder/builder.go`](../internal/builder/builder.go) resolves the
app's `appTree` in sorted entry order (`tree.Sorted()`), then calls
`format.WriteLayer(layerTmp, layerTarTmp, r.Layer.Format(), appTree.WriteTar, prefetch)`
in [`internal/format/format.go`](../internal/format/format.go):

1. `appTree.WriteTar` streams directories and regular files through
   `format.NewLayerWriter` (`tar.FormatPAX`, directories `0755`, files `0644`,
   `ModTime` set to Unix epoch zero). Unsafe paths fail with `ErrTarBadPath`,
   and symlinks fail with `ErrTarSymlinkUnsupported`.
2. For `FormatTar`, `WriteLayer` writes the deterministic tar directly and
   computes its SHA-256 and size.
3. For `FormatGzip`, `WriteLayer` pipes the tar through `format.GzipWriter`
   (`GzipLevel = 6`, zero `ModTime`, `OS = 255`).
4. For `FormatEstargz`, `WriteLayer` buffers the tar to `layerTarTmp`, rewinds
   it, and calls `estargz.Convert(r, cw, prefetch)` while hashing the written
   blob:
   - `Convert` checks the tar stream for reserved names (`estargz.TOCTarName`,
     `estargz.PrefetchLandmark`, `estargz.NoPrefetchLandmark`) and rejects them
     with `ErrReservedName`.
   - It passes the tar file to containerd's `estargz.Build` with
     `WithChunkSize(DefaultChunkSize)` (`4 MiB`),
     `WithCompressionLevel(6)`, and—when `prefetch` is non-empty—
     `WithPrioritizedFiles(prefetch)` and `WithAllowPrioritizeNotFound`.
   - `Blob.Descriptor` attaches the resulting annotations
     (`estargz.TOCJSONDigestAnnotation` and
     `estargz.StoreUncompressedSizeAnnotation`) onto the manifest layer
     descriptor.

### Prioritized files

Recipes with `"layer": "estargz"` can declare an optional `"prefetch"` array
listing relative paths inside the app layer that should be placed ahead of the
rest of the archive ([`recipes.md`](recipes.md)):

- [`internal/recipe/recipe.go`](../internal/recipe/recipe.go)
  (`validatePrefetch`) rejects `"prefetch"` unless `"layer": "estargz"`,
  requires `/` separators with no leading `/`, `\`, `//`, `:`, `.`, or `..`
  segments, and enforces case-insensitive uniqueness.
- [`internal/builder/builder.go`](../internal/builder/builder.go) resolves each
  entry against `appTree.FilePath(p)` (preserving the tree's canonical casing),
  logs a warning and skips any path missing from the built tree, and passes the
  matched paths into `format.WriteLayer` -> `estargz.Convert`.

[`scripts/record-prefetch.sh`](../scripts/record-prefetch.sh) documents the
maintainer workflow for generating a `"prefetch"` list by building a recipe,
pushing to a local registry, pulling with `--lazy` (`QUASIDOCK_TEST_LAZY_MIN=1`,
`QUASIDOCK_TEST_LAZY_NO_HYDRATE=1`), running target commands, and parsing
`fetched ...: <n> bytes, for <exe>` entries from `lazy/<hex>/log`. Note that
in the current Go port, `internal/lazy/helper.go` does not yet emit those log
lines or honor `QUASIDOCK_TEST_LAZY_NO_HYDRATE` / `QUASIDOCK_TEST_LAZY_GATE`
(see [Pulling](#pulling)).

### Recipes

Most recipes omit `"layer"` and default to `"gzip"`, avoiding the ~11% eStargz
framing overhead on small tools and shared base layers. Six recipes in
[`recipes/`](../recipes) opt into `"layer": "estargz"`:

- [`recipes/llvm-mingw.json`](../recipes/llvm-mingw.json): `"layer": "estargz"`.
- [`recipes/qt6-sdk-base.json`](../recipes/qt6-sdk-base.json):
  `"layer": "estargz"`, built on `from` base layers `cmake` (`gzip`),
  `llvm-mingw` (`estargz`), and `busybox` (`gzip`).
- [`recipes/qt6-sdk.json`](../recipes/qt6-sdk.json): `"layer": "estargz"` plus
  456 `"prefetch"` paths.
- [`recipes/qt6.json`](../recipes/qt6.json): `"layer": "estargz"` plus 36
  `"prefetch"` paths (consuming `qt6-sdk` in `sdk`).
- [`recipes/texlive.json`](../recipes/texlive.json): `"layer": "estargz"`,
  built on `from` base layers `perl` and `gs` (`ghostscript`), both `gzip`.
- [`recipes/vscodium.json`](../recipes/vscodium.json): `"layer": "estargz"`,
  built on `from` base layer `git` (`mingit`, `gzip`).

Downstream GUI apps such as [`recipes/flameshot.json`](../recipes/flameshot.json)
and [`recipes/notes.json`](../recipes/notes.json) keep default `"gzip"` for
their own small app layer while still reusing `qt6`'s eStargz base layer via
`from`.

### Pulling

#### Normal pull (`quasidock pull <image>`)

[`internal/remote/remote.go`](../internal/remote/remote.go) (`DownloadBlobs`)
downloads missing layer blobs concurrently (up to `MaxBlobWorkers = 4` workers).
If a missing layer has `FormatOfDescriptor(desc).Indexed()` (`FormatEstargz`)
and locally stored images already contain indexed blobs, `downloadByPlan`
attempts delta assembly (see [Updating](#updating-delta-range-assembly)) before
falling back to whole-blob download (`reg.DownloadBlob`, which retries broken
streams via HTTP `Range` up to `registry.MaxResumes = 5` times). Once blobs are
in the store, `lifecycle.Install` calls `store.Deploy` -> `format.Deploy`:

- `FormatTar` runs `format.Extract`.
- `FormatGzip` runs `format.Inflate` then `format.Extract`.
- `FormatEstargz` opens the blob with `estargz.Open`, verifies
  `r.TOCDigest()` against `desc.Annotations[estargz.TOCJSONDigestAnnotation]`,
  validates paths and entry types via `format.CheckDeployable`, and calls
  `r.Extract(destPath, MaxExtractWorkers)`.

#### Lazy pull (`quasidock pull --lazy <image>`)

`quasidock pull --lazy` (alias `quasidock image pull --lazy`, with `--yes`
accepted as a no-op; see
[`experiment-cloud-files.md`](experiment-cloud-files.md) for the Cloud Files
design) marks eligible layers via
[`internal/lazy/lazy.go`](../internal/lazy/lazy.go):

- `lazy.Eligible(desc)` returns true only when `FormatOfDescriptor(desc)` is
  `Indexed()` and `desc.Size >= DefaultMinSize` (32 MiB, overrideable with
  `QUASIDOCK_TEST_LAZY_MIN`). Smaller or non-eStargz layers download in full.
- `lazy.Prepare` opens a `remote.RemoteBlob` probing the tail (`TailLen(size)`
  is `size / 64`, clamped to `[16 KiB, 1 MiB]`). If the registry ignores
  `Range` and returns HTTP `200` (`registry.ErrRangeIgnored`), `Prepare` returns
  `PreparedWhole: true` and falls back to normal pull.
- Otherwise `Prepare` parses the TOC via `estargz.Open`, runs
  `format.CheckDeployable`, verifies every regular file has a digest, writes
  `lazy/<hex>/tail`, `layer.json`, and `state.json`, takes a shared
  `lazy/<hex>.pull.lock`, and launches a background
  `quasidock lazy-helper <store> <hex>` process from a cached binary copy
  (`lazy/quasidock-<hash8>.exe`).
- On `quasidock run`, `container start`, and `builder.ResolveDependency`,
  `lazy.EnsureServed` restarts any missing helper for active lazy layers, and
  `internal/run/run.go` checks `lazy.FailedFetches` to report fetch errors if a
  container exits non-zero.

> **Current Go helper scope.** [`internal/lazy/blobreader.go`](../internal/lazy/blobreader.go)
> provides sparse-file (`FSCTL_SET_SPARSE`) block tracking (`blob.sparse`, 1 MiB
> `have` bitmap with header `QDBLOB01`, 3 download workers, 4 MiB demand pieces
> vs. 8 MiB background pieces after a 1000 ms quiet window), and
> [`internal/cloudfiles/cloudfiles.go`](../internal/cloudfiles/cloudfiles.go)
> wraps `cldapi.dll` (`CfRegisterSyncRoot`, `CfCreatePlaceholders`,
> `CfConnectSyncRoot`, `CfExecute`, `CfHydratePlaceholder`, plus
> `ErrUnsupported` for Dev Drive and unsupported filesystems). However,
> [`internal/lazy/helper.go`](../internal/lazy/helper.go) (`RunHelper`)
> currently registers the sync root, creates placeholders, waits on
> `pull.lock`, downloads the full blob via `reader.DownloadAll()`, verifies its
> SHA-256, deploys into a temporary directory, unregisters the sync root, and
> swaps the directory once unused—it does not yet wire `CfConnectSyncRoot`
> `FETCH_DATA` callbacks, pre-hydrate `Index.Prioritized()`, or implement the
> test hooks expected by [`tests/lazy.sh`](../tests/lazy.sh) and
> [`scripts/record-prefetch.sh`](../scripts/record-prefetch.sh).

### Updating (delta Range assembly)

When pulling an `Indexed()` layer, `remote.downloadByPlan` in
[`internal/remote/remote.go`](../internal/remote/remote.go) builds a delta
transfer plan across existing local layers:

1. Collect all other `Indexed()` layer blobs present in the local store across
   installed images (`indexedLayers`), and open each with `estargz.Open`.
2. Open the new blob's tail with `remote.OpenRemoteBlob` and parse its TOC with
   `estargz.Open`.
3. Call `estargz.MakePlan(newReader, olds, desc.Size)` in
   [`internal/estargz/plan.go`](../internal/estargz/plan.go):
   - `MakePlan` indexes every member with `Offset > 0` across `olds` by the
     tuple `(digest, compressedLen, uncompressedSize)`—where `digest` is
     `ChunkDigest` or `Digest`, `compressedLen` is `NextOffset() - Offset`, and
     `uncompressedSize` is `ChunkSize` or `Size`.
   - Matching entries in `newReader` become `PieceCopy` spans pointing to the
     old blob and byte offset; uncovered byte intervals (new files, modified
     chunks, tar headers, TOC, and footer) become `PieceFetch` spans.
   - If `plan.Copied * 10 < desc.Size * MinReuseTenths` (less than 10% reuse),
     `downloadByPlan` aborts the plan and downloads the whole blob instead.
4. Coalesce requests via `plan.Coalesce(newBlob.TailStart)`:
   - Removes/trims any piece past `TailStart` (already downloaded in
     `newBlob.Tail`).
   - Merges adjacent `PieceFetch` spans separated by `PieceCopy` gaps of at most
     `estargz.MinGapSize` (64 KiB) so tiny reusable members do not generate
     extra HTTP requests.
   - While `plan.NumRequests() > estargz.MaxRequests` (64), repeatedly merges the
     smallest `PieceCopy` gap.
5. `downloadByPlan` executes all `PieceFetch` requests concurrently (up to
   `MaxBlobWorkers = 4` workers), copies `PieceCopy` spans from local blobs,
   writes `newBlob.Tail` at `TailStart`, hashes the assembled file end-to-end,
   and verifies `at == desc.Size && hashHex == expectedHex` before calling
   `st.PutBlobFile`. Any Range refusal (`ErrRangeIgnored`) or hash mismatch
   falls back cleanly to a full `reg.DownloadBlob` transfer.

For scale context, Zigsaw rebuilt its 196 MiB `mingw` layer with one edited
file and reused 99.6% of the existing blob across ~4 Range requests fetching
under 1 MiB ([Zigsaw iteration 66](../legacy/iteration-66.md); also tested in
[`tests/registry.sh`](../tests/registry.sh)).

### Reading one file

Two CLI subcommands in [`internal/cli/images.go`](../internal/cli/images.go)
inspect `Indexed()` images via [`internal/peek/peek.go`](../internal/peek/peek.go)
without pulling full layers:

- `quasidock image files <image>` (`peek.List`): resolves `<image>` (local or
  remote), walks layers from the app's own top layer down through base `from`
  layers, opens each `Indexed()` layer via `remote.OpenRemoteBlob` and
  `estargz.Open`, and lists regular files (`estargz.IsLayerFile`). On a
  multi-layer image, non-indexed base layers are noted; if a single-layer image
  is not indexed, `image files` returns an error.
- `quasidock image cat <image> <path>` (`peek.Cat`): cleans `<path>` to a
  forward-slash relative path, searches `Indexed()` layers top-down with
  `reader.Lookup` and `reader.OpenFile`, and streams only that entry's gzip
  member(s) over HTTP `Range` to `stdout`. Non-indexed layers are skipped (and
  named if the path is not found in any indexed layer).

### Clients that don't know a format

Two failure modes are handled separately:

- **Unrecognized layer media type.** `format.FormatOfDescriptor` returns
  `(FormatUnknown, false)` and `oci.ValidateLayers` rejects unknown types such
  as `tar+zstd` rather than silently misinterpreting bytes.
- **Unindexed reader pulling an eStargz layer.** Standard pullers see
  `oci.MediaTypeLayerTarGzip` and unpack every gzip member sequentially. Inside
  Quasidock itself, `FormatOfDescriptor` always detects
  `estargz.TOCJSONDigestAnnotation` and dispatches through `deployEstargz`,
  which strips eStargz landmark/TOC entries (`estargz.IsLayerFile`).

### Not in this design

- **Non-regular entries and symlinks.** `format.LayerWriter` rejects symlinks
  (`ErrTarSymlinkUnsupported`), and `format.CheckDeployable` accepts only
  regular files and directories (`"reg"` and `"dir"`).
- **Zstd (`tar+zstd`) or `zstd:chunked`.** Only `MediaTypeLayerTar` and
  `MediaTypeLayerTarGzip` are valid in manifests and recipes.
- **Custom OCI media types.** Reusing `MediaTypeLayerTarGzip` keeps registry
  push, pull, and cross-client compatibility without custom manifest handling.
- **Cross-image layer deduplication inside single layers.** Shared frameworks
  and toolchains use multi-layer `from` dependencies; delta Range plans operate
  across versions or similar `Indexed()` layers already in the local store.

## Decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| Media type | `MediaTypeLayerTarGzip` + `TOCJSONDigestAnnotation` | Preserves standard registry compatibility and multistream gzip fallback |
| eStargz implementation | Wrap `containerd/stargz-snapshotter/estargz` in `internal/estargz` | Avoids maintaining a custom Zig/C codec (`stargazer`) and isolates library details |
| Chunk size & compression | `DefaultChunkSize = 4 MiB`, deflate level `6` | Keeps framing overhead near ~11% on large toolchains while bounding random-read size |
| Layer format selection | Default `"gzip"`; opt-in `"layer": "estargz"` per recipe | Avoids overhead on tiny layers while giving big SDKs/apps peek, delta, and lazy support |
| Prefetch configuration | Optional `"prefetch"` allowlist in recipe JSON (requires `"estargz"`) | Keeps cold-start layout declarative; missing entries warn instead of breaking builds |
| Delta transfer policy | Reuse threshold `>= 10%` (`MinReuseTenths = 1`), merge gaps `<= 64 KiB`, cap `64` requests, verify full SHA-256 | Prevents pathological Range fragmentation and guarantees byte-exact blobs on disk |
| Lazy eligibility & fallback | `DefaultMinSize = 32 MiB`, whole-blob fallback on `ErrRangeIgnored` | Skips Cloud Files setup on small blobs or registries without `Range` support |
