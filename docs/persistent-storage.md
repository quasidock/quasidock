# Persistent storage

Named volumes reuse data across containers and image versions. Mount the complete
`${data}` tree to preserve profile, cache, and registry state, or a subpath to
persist selected application files. Quote mount arguments with single quotes in
PowerShell so `${data}` reaches Quasidock literally.

```powershell
quasidock volume create node-state
quasidock run --rm -v 'node-state:${data}' node node.exe -e 'require("fs").writeFileSync(process.env.HOME + "/saved.txt", "hello")'
quasidock run --rm -v 'node-state:${data}' node node.exe -e 'console.log(require("fs").readFileSync(process.env.HOME + "/saved.txt", "utf8"))'
quasidock run --rm --mount 'type=bind,source=C:\work,target=${data}/workspace' node node.exe --version
quasidock volume ls
quasidock volume inspect -f '{{.Mountpoint}}' node-state
quasidock volume rm node-state
```

`volume create NAME` is idempotent and volume names resolve case-insensitively.
`run` automatically creates missing named volumes. In `-v`, absolute and explicitly
relative paths select binds; other sources select named volumes. Bind directories
must already exist. Relative binds resolve against the caller's original working
directory, including detached runs. Use `--mount type=bind` to explicitly bind a
relative directory whose name could also be a volume name. Fields containing commas
can be CSV-quoted, for example `'type=bind,"source=C:\work, files",target=${data}/workspace'`.

Mounts support writable directories on local NTFS volumes. Destinations are
`${data}` or `${data}/PATH`; multiple destinations must be disjoint. A root mount
cannot be combined with subpath mounts. Arbitrary absolute destinations, read-only
mounts, files, remote shares, anonymous volumes, and volume drivers are unsupported.
Mounts do not copy image files into new volumes. Multiple running containers may
share storage; applications coordinate their concurrent writes. Existing HKCU key
conflicts still apply when containers use different backing registry directories.

Container inspection includes normalized `HostConfig.Mounts` and resolved
`Mounts` with `Type`, `Name` (for volumes), `Source`, `Destination`, and `RW`.
Named volumes live under `volumes/<name>/data`, separate from container metadata.
Any retained container record pins its named volumes, including exited containers
and failed launches. Remove those containers before removing their volumes.

`--rm`, `rm`, `container prune`, image removal, and ordinary `prune` (including
`--data`) preserve named volumes and bind contents. Use `volume rm NAME...` or
`volume prune` to delete unreferenced named volumes; `volume prune --dry-run`
reports candidates without deleting them. Bind contents are never collected.
`repair` restores mount permissions left behind after a supervisor crash while
preserving access required by live consumers.

## Inspect private data changes

`quasidock diff CONTAINER` and `quasidock container diff CONTAINER` compare private
data with the pinned image's initial data. Output contains sorted `A PATH`,
`C PATH`, and `D PATH` lines; `/home/file.txt` means
`${data}/home/file.txt`. The data root itself is omitted. Runtime-created profile,
cache, and registry files are included, with registry hives reported as files
rather than individual keys.

Ordinary images have an empty baseline: editing an added file leaves it `A`;
deleting it removes the entry. Committed images compare against their data
snapshot, reporting additions, changed file contents or entry types, and
deletions. Timestamps, ACLs, and case-only spelling changes do not count.
Image deployments, host filesystem grants, and container metadata/logs outside
`${data}` are excluded.

Mount destinations and their contents are excluded even when their junctions
are detached. A mount at `${data}` produces empty output. Other reparse points
are listed as leaf entries; their targets are never enumerated.

Diff accepts a container name, full ID, or unique ID prefix, for created, running,
paused, and exited containers. A running application can continue writing during
the scan, so results are best effort. For ordinary images disappearing descendants are skipped;
missing data roots, permission failures, and other scan errors fail the command
without printing scan results. Success returns exit code 0, including when there
are additions or the output is empty.

## Commit private data

`quasidock commit [OPTIONS] CONTAINER [REPOSITORY[:TAG]]` (also `container commit`)
captures the source app, its saved command/environment overrides, and private
data into a runnable image. Names, full IDs, and unique prefixes select created,
running, paused, or exited containers. A named target defines the app ID from
the final repository component and the version from its tag (default `latest`),
and becomes that app ID's local default. Without a target, the result is unnamed
and does not move any default or tag. Success prints only its full manifest digest.

Options precede CONTAINER:

| Option | Behavior |
|---|---|
| `-p`, `--pause[=true|false]` | Temporarily freeze running application processes; default true |
| `-a`, `--author TEXT` | Record author metadata |
| `-m`, `--message TEXT` | Record a commit message |
| `-c`, `--change INSTRUCTION` | Apply a configuration change; repeatable, in order |

Capture pauses only the application job. A supervisor-owned lease restores its
previous pause state after capture errors, Ctrl+C, or a disconnected/killed
commit client. Already-paused sources remain paused. Older supervisors need
stop/start with the current binary for automatic pausing, or `--pause=false`.
Compression happens after capture so the application can resume sooner.
`--pause=false` is best effort: concurrent writes, disappearing files, and
exclusive file locks can fail capture. No image reference is published on a
capture failure. A failed pause restoration terminates the affected job rather
than leaving it silently frozen.

Mount destinations are excluded even while detached, including private files
under those destinations. A root mount produces an empty private-data snapshot.
Declared HKCU keys are copied into a fresh private hive, reading live changes
only after verifying their container ownership. Registry state backed by a root
or registry-directory mount is excluded. Mounts inside the hive directory are
rejected. Unconfigured reparse points and unreadable files fail capture.

`create` and `run` initialize a fresh private data tree before publishing the
container record, without hardlinks or copied ACLs. Mounted subtrees are omitted
without copying files into volumes or binds. Starts/restarts never reseed.
Recommitting saves the complete current private tree and replaces the old data
layer, so deleted files do not return.

Changes support `ENV` assignments and `LABEL` key/value pairs with quoting.
Values preserve Quasidock placeholders; no build-time environment expansion or
Linux path translation runs. `CMD` and `ENTRYPOINT` require JSON string arrays.
Replacing ENTRYPOINT clears CMD; supply a later CMD to set defaults. An empty
ENTRYPOINT is valid when CMD supplies a command. Explicit shells can be named
in JSON, such as `ENTRYPOINT ["cmd.exe", "/c"]`. `WORKDIR` resolves relative to
the preceding image workdir, accepts app-relative or `${data}` paths, and must
name an existing directory. Other instructions are rejected.

Host mounts, host cwd, sandbox/network overrides, port assignments, and terminal
preferences are not baked into the result. Parent image permissions remain.
Saved environment overrides include explicitly absent variables; subsequent
run overrides still take precedence. Arbitrary file contents are preserved,
so embedded absolute paths to the source container may require reconfiguration.

Committed images use v4 config metadata and require a compatible binary. Existing
stores need no migration. They support normal tag/push/pull, inspection, and
pruning; `push --sources` and using committed images as recipe dependencies are
unsupported because snapshot state does not describe a reproducible recipe.
