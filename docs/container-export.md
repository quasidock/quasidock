# Container filesystem export

`quasidock export` (also `quasidock container export`) writes a container's pinned
app, runtimes, and current private data to an uncompressed tar archive:

```powershell
quasidock export -o worker.tar worker
quasidock container export --output=worker.tar --pause=false worker
```

Syntax: `export [-o FILE|--output FILE] [-p|--pause[=true|false]] CONTAINER`.
Options precede the single container argument. Names, full IDs, and unique ID
prefixes select created, running, paused, or exited containers. `-h` and `--help`
show command help.

## Archive layout

```text
app/                           pinned image's application files
data/                          current private data, including profile and cache
runtimes/runtime-<alias>/       each declared runtime's files
```

All three top-level directories are present, including when empty. Runtime
aliases come from the image configuration; the `runtime-` prefix keeps aliases
such as `con` usable as Windows directory names. Runtime layers are matched by
digest, including older images with a different manifest order. Shared runtime
layers appear under each applicable alias.

Private data comes from the current container, without overlaying its original
image seed: deleted seeded files stay absent. Mount destinations are excluded,
including private files hidden beneath detached mounts. Named volumes and bind
contents are excluded. A root data mount produces an empty `data/` directory.

The archive preserves regular-file bytes and empty directories. Paths are sorted;
permissions are normalized to `0644` for files and `0755` for directories, with
zero timestamps and numeric owner/group IDs. Source modification times, owners,
ACLs, alternate streams, and Windows attributes are not preserved. Links,
unconfigured reparse points, special files, and unsafe paths fail export.

## Capture and output

Export works offline. The pinned manifest, config, and required app/runtime blobs
must be local and pass hash and size checks. An incomplete lazy image fails before
capture with guidance to fully pull the exact repository digest. Export reads
complete blobs into private staging rather than hydrating existing deployments.

By default, running application processes pause only while private data and
declared HKCU keys are captured. HKCU keys are copied into a fresh private hive
after checking container ownership. Registry state covered by a root or registry
directory mount is excluded; mounts inside a captured hive directory are rejected.
Already-paused containers remain paused. A supervisor-owned capture lease restores
the previous pause state on errors, Ctrl+C, or client disconnection. Older
supervisors require stop/start with the current binary, or `--pause=false`.

Applications resume before tar output starts. `--pause=false` provides best-effort
capture: concurrent changes, disappearing files, permissions, and exclusive file
locks can fail export. Pausing does not flush application buffers or coordinate
external writers.

Output defaults to binary stdout; `-o -` also selects stdout. Diagnostics go to
stderr, and successful file exports print nothing. Prefer `-o FILE` in PowerShell
to avoid shell text conversion. File output uses a sibling temporary file and
replaces an existing destination only after the complete archive is synced and
closed. Destinations inside the source data, blob, deployment, or temporary-store
trees are rejected. Errors clean up staging; failed stdout output can be partial
and must be discarded. Success returns 0, failure 1, and Ctrl+C 130 after cleanup.

## Uses and limits

The archive is for manual filesystem extraction. It contains no launcher, launch
configuration, OCI manifest, container record, logs, host grants, or mount
contents. It cannot be restored with `load`, and extraction does not automatically
make the application portable; embedded absolute paths may need reconfiguration.

Use `commit` followed by `save` when transferring a runnable Quasidock image.
Export does not register an image, change image defaults, or require a store or
supervisor format migration.
