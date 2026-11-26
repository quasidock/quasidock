# Image import from a filesystem export

`quasidock image import` (also `quasidock import`) creates an image from a
Quasidock container export and separately supplied app configuration:

```text
quasidock image import --config FILE [-c INSTRUCTION...] [--force] FILE|- [REPOSITORY[:TAG]]
```

Options precede the archive argument. Input is a local file or explicit `-` for
stdin. Plain tar and gzip-compressed tar are detected by their contents. Import
works offline and prints only the full manifest digest on success; diagnostics
go to stderr. Success returns 0, failure 1, and Ctrl+C 130 after cleanup.

## Export, configure, and import

Export contains filesystem bytes without launch configuration. Save the original
image's configuration separately, then adjust it for the intended launch:

```powershell
quasidock export -o worker.tar worker
$image = quasidock container inspect -f '{{.Image}}' worker
$config = quasidock image inspect -f '{{json .Quasidock}}' $image
[IO.File]::WriteAllText((Join-Path $PWD 'worker-config.json'), ($config -join "`n"), [Text.UTF8Encoding]::new($false))
quasidock image import --config worker-config.json -c 'LABEL imported=true' worker.tar example.test/worker:restored
quasidock run --rm --pull=never example.test/worker:restored
```

The configuration describes the image's launch defaults. Container-specific
command, environment, workdir, and permission overrides are not part of export;
include any desired overrides in the JSON or `--change` instructions. Prefer
file arguments in PowerShell to keep binary archives away from text pipelines.

`--config` requires a local UTF-8 app-config JSON file, matching the `.Quasidock`
object above, rather than the full inspect response or a build recipe. It supplies
`id`, `version`, `entrypoint`/`cmd`, `workdir`, `path`, `env`, permissions, registry
declarations, and any `from` runtime metadata. A minimal configuration is:

```json
{
  "id": "worker",
  "version": "1",
  "entrypoint": ["worker.exe"]
}
```

Repeated `-c` or `--change` instructions apply in order after reading the config.
Supported instructions match `commit`: `ENTRYPOINT` and `CMD` require JSON string
arrays; `ENV`, `WORKDIR`, and `LABEL` use the same syntax and placeholder rules.
Setting `ENTRYPOINT` clears `CMD`. At least one final entrypoint or command is
required. A configured working directory must exist in the imported app or data.

## Files, layers, and registration

The input must use the [container export layout](container-export.md):

```text
app/
data/
runtimes/runtime-<alias>/
```

All three root directories must be present, including when empty. Every runtime
alias in `from` must have its matching directory; additional runtime directories
are rejected. Runtime IDs, versions, original image digests, paths, environment,
and port uses come from the config. Runtime image metadata is supplied provenance,
not signature verification or proof of the exported bytes' origin.

Import preserves regular-file bytes and empty directories. It writes canonical
gzip layers with normalized permissions and zero timestamps, one per runtime
alias, followed by the private-data seed and application layer. Identical blobs
are shared. Existing `dataLayer` and runtime `layer` fields are replaced with
digests computed from those trees, and the resulting configuration uses v4.
The same trees and final config produce the same image ID regardless of archive
entry order, timestamps, gzip wrapping, input filename, or target alias.

An optional target adds a local alias using the usual reference rules, including
an implicit `latest` tag. It does not change configured ID or version. Digest
targets are rejected. Without a target, import creates no tag. A conflicting alias
requires `--force`; replacement keeps previous images and container selections.
Existing app defaults remain selected. An app without a default selects the
imported image. Repeated imports preserve local source provenance and cache dates,
merging repository provenance as aliases are added.

Import registers the image without deploying files, launching a program, or
activating captured HKCU state. Subsequent create/run operations use the usual
independent writable data copies and declared registry handling. Mount contents,
container records, and other state omitted by export cannot be recovered.

## Validation and limits

Import validates and stages the complete archive before publishing its catalog
entry. It rejects unknown roots, unsafe Windows paths, case-insensitive duplicate
members, file/directory conflicts, links, special files, truncated archives,
invalid gzip checksums, and nonzero data after tar end markers. Config and generated
JSON records are limited to 16 MiB each; file contents are streamed to disk.
Source owners, ACLs, timestamps, alternate streams, and Windows attributes are
not restored.

Failures before publication leave the catalog unchanged. A filesystem error while
publishing may leave verified, unreferenced blobs for pruning. Temporary staging
is removed when the command returns, including on Ctrl+C.

URLs, generic filesystem tar layouts, Docker archives, and outer compression
other than gzip are unsupported. `load` continues to accept OCI image archives
produced by `save`. Use `commit` followed by `save` when transferring a runnable
container configuration without supplying a separate config file. Imported images
can subsequently be transferred with `save`/`load` or published with `push`.
