# Image archives

`quasidock save` and `quasidock load` transfer local Quasidock images without a
registry. Both commands also exist under `quasidock image`.

```text
quasidock save [-o FILE|--output FILE] IMAGE [IMAGE...]
quasidock load [-i FILE|--input FILE] [-q|--quiet] [--force]
```

Options precede image arguments. Save defaults to stdout and load defaults to
stdin; an explicit `-` output or input also selects the corresponding stream.
Save produces uncompressed tar. Load detects gzip by its header, including when
reading stdin. Diagnostics use stderr; successful loads list restored names, or
the manifest ID for unnamed images, unless quiet mode is enabled. On Windows,
prefer `-o` and `-i` to keep shell text conversion away from binary streams.

## Image selection and restoration

Save uses the usual local resolver: an app ID selects its current default, an
explicit name selects its cached reference, and a full `sha256:...` selects that
manifest. It includes every local alias for each selected manifest, even when
selection used only one alias or the digest. Duplicate selections are combined.
It includes all runtime and committed-data layers referenced by those manifests,
without adding separate base-image manifests or other cached app versions.

Load checks the entire archive before registering images. An incoming name
already assigned to a different digest fails the import with guidance to use
`--force`. With that option, incoming names move while prior images and existing
containers retain their digests. Existing app defaults always remain selected.
For an app without a default, load uses its archived default if included;
otherwise it selects the lexicographically smallest imported manifest digest.
It does not invent tags for unnamed images. Repeated loads preserve existing
source provenance and merge repository provenance.

Local cache dates (`image inspect`'s `CachedAt`) are not archived. A newly loaded
manifest receives the destination store's registration time; repeat loads
preserve its existing local cache date. An older undated record receives a date
when successfully loaded again.

Layers are deployed when subsequently used by create/run paths. Loading does not
launch a program or activate a captured HKCU hive. Images committed with private
data receive the same independent writable data copies as other committed images.

Save never completes a lazy download. If any required blob is missing, fully
pull the exact repository digest shown in the error and retry. Both save and load
verify blob hashes and sizes. A corrupt existing destination blob causes load to
fail rather than silently reusing it.

File saves use a temporary sibling file and replace the destination only after
the complete archive is written and synced. Failed imports leave the image
catalog unchanged. A filesystem error while publishing a validated import can
leave verified, unreferenced blobs for normal pruning. Temporary staging is
removed when the command returns; interrupted stdout streams can be partial.

## OCI layout and metadata

The tar contains an [OCI image layout](https://github.com/opencontainers/image-spec/blob/v1.1.1/image-layout.md):

```text
oci-layout
index.json
quasidock.json
blobs/sha256/<hex digest>
```

`oci-layout` contains `{"imageLayoutVersion":"1.0.0"}`. The schema-version-2
`index.json` lists flat OCI image manifest descriptors for `windows/amd64`.
Each alias has its own descriptor using the `org.opencontainers.image.ref.name`
annotation, containing the full repository/tag or repository/digest reference.
An unnamed image has an unannotated descriptor. Manifests, configs, and layers
are copied byte for byte and stored once per digest. Entry order and tar metadata
are deterministic for an unchanged selection and catalog snapshot.

`quasidock.json` is an optional extension containing only provenance and defaults
for included images:

```json
{
  "version": 1,
  "images": {
    "sha256:<manifest digest>": {
      "source": "original registry reference or local recipe path",
      "repositories": ["example.test/app"]
    }
  },
  "defaults": {
    "app-id": "sha256:<manifest digest>"
  }
}
```

`source` and `repositories` may be omitted. App IDs and versions come from the
image configs. Unknown metadata versions and references to absent images are
rejected. Layouts without this extension can load with the deterministic default
fallback. Additional regular files and unknown auxiliary descriptor types are
ignored; unreferenced blobs are not published to the destination store.

## Limits

- Supported archives contain flat indexes with Quasidock image configs and the
  layer formats Quasidock already supports: tar, gzip, and eStargz. Nested indexes,
  other platforms, general Docker images, and Docker save archives are unsupported.
- Archives must include every referenced blob, even if the destination already
  has it. JSON members, manifests, and configs are limited to 16 MiB each; layer
  blobs are streamed rather than held in memory.
- Traversal and absolute paths, links, special files, duplicate recognized tar
  members, descriptor inconsistencies, and truncated or corrupt data are rejected.
  Archive paths are never extracted directly onto the filesystem.
- Container records, live private data, volumes, credentials, signatures, source
  attachments, and build caches are excluded. Committed private files and declared
  HKCU state travel in their image data layer. Repository/source metadata records
  provenance and does not establish signature verification.
- Save does not compress the outer archive. Load accepts plain tar and gzip;
  bzip2, xz, and zstd outer compression are unsupported.
