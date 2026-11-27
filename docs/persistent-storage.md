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
mounts, files, remote shares, and volume drivers are unsupported.
Mounts do not copy image files into new volumes. Multiple running containers may
share storage; applications coordinate their concurrent writes. Existing HKCU key
conflicts still apply when containers use different backing registry directories.

Container inspection includes normalized `HostConfig.Mounts` and resolved
`Mounts` with `Type`, `Name` (for volumes), `Source`, `Destination`, and `RW`.
Named volumes live under `volumes/<name>/data`, separate from container metadata.
Any retained container record pins its volumes, including exited containers
and failed launches. Remove those containers before removing their volumes.

`--rm`, `rm`, `container prune`, image removal, and ordinary `prune` (including
`--data`) preserve named volumes and bind contents. Use `volume rm NAME...`
to remove specific unused volumes. Use `volume prune --all` to collect unused
named and anonymous volumes. `volume prune --dry-run` previews candidates.
Volume cleanup preserves host bind contents.
`repair` restores mount permissions left behind after a supervisor crash while
preserving access required by live consumers.

## Filter and format volume listings

Set labels on first creation with repeatable `--label KEY[=VALUE]` options
before the name. A bare key sets an empty value. For repeated keys, the last
value wins. Creating an existing volume again prints its name and keeps its
saved labels. Use `volume inspect NAME` to see the labels.

```powershell
quasidock volume create --label team=build --label cached build-cache
quasidock volume ls --filter 'name=^build-' --filter 'label=team=build'
quasidock volume list --filter dangling=true --format json
quasidock volume ls --format 'table {{.Name}}\t{{.Labels}}'
```

`-f` and `--filter` accept `name=REGEXP` (case-sensitive), `driver=DRIVER`
(currently `local`), `dangling=true|false` (also `1` or `0`), and
`label=KEY[=VALUE]`. Labels match by exact key and, when supplied, exact
value. Repeated values for the same filter key use OR. Different keys use AND.
Dangling volumes have no references in saved container records, including
stopped containers. Volume deletion also checks live mount grants.

`--format table` selects the default table, and `table TEMPLATE` adds headers
to a custom table. A plain Go template renders once per matching volume.
`--format json` emits one JSON object per line. Fields are `Name`, `Driver`,
`Scope` (`local`), `Mountpoint`, `CreatedAt`, `Anonymous`, and `Labels`.
`Labels` is a map. Use `{{.Label "key"}}` to look up a label and use its key
as the column heading in a custom table. Use `{{json .}}` to encode a row.
`-q` and `--quiet` take precedence over formatting and print only matching names.

## Anonymous volumes

Omit the source to allocate a volume with a generated name. Use the same syntax
with `run` or `create`.

```powershell
quasidock create --name worker -v '${data}/work' node
quasidock create --name other --mount 'type=volume,target=${data}/work' node
quasidock rm --volumes worker other
```

Use `rm -v` or `rm --volumes` to remove the container and its anonymous
volumes. Add `-f` to stop a running container first. Plain `rm` retains its
volumes. Automatic `run --rm` cleanup also removes eligible anonymous volumes.
Any saved reference or live mount grant keeps a volume. A volume mounted
explicitly by its generated name is treated as a named attachment for cleanup.
Named volumes and bind contents survive container removal.

Find generated names in `inspect` or `volume ls`.

## Prune unused volumes

`volume prune` removes unused anonymous volumes by default, including any
left behind after a supervisor crash. Use `-a` or `--all` to include unused
named volumes. Saved container references and live mount grants protect
volumes. Add `--dry-run` to list the candidates and estimated reclaimable bytes.

Use repeatable `--filter` options to select volumes by label. `label=KEY`
requires the key, and `label=KEY=VALUE` requires an exact value. Use
`label!=KEY` or `label!=KEY=VALUE` to protect matching volumes. Every inclusion
must match, and any matching exclusion protects a volume. Label keys and
values are case-sensitive. A trailing `=` matches an explicitly empty value.

```powershell
quasidock volume prune --all --dry-run --filter 'label=team=ops' --filter 'label!=keep'
```

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

## Copy files

`quasidock cp SOURCE DESTINATION` and `quasidock container cp SOURCE DESTINATION`
copy regular files and directories between one container and the host. Container
names, full IDs, and unique ID prefixes work in `CONTAINER:PATH`. `/home/file`,
`home/file`, and `home\file` all refer to `${data}/home/file`; `/` and `.` select
the data root's contents. Paths do not resolve against the application's workdir
and do not expand placeholders or environment variables.

```powershell
quasidock cp .\config.json worker:/home/config.json
quasidock cp worker:/home/config.json .\saved.json
quasidock cp '.\project\.' worker:/workspace
quasidock cp worker:/workspace .\workspace-backup
```

Host relative paths resolve against the current CLI working directory. Drive
paths (`C:\work\file`, `C:/work/file`, and `C:relative`), UNC paths, and explicitly
relative paths remain host paths. A single-letter container name conflicts with
drive syntax; use its container ID. Use `--` before operands beginning with `-`.
Container paths reject `..`, drive/UNC paths, Windows device names, alternate
streams, and names ending in spaces or dots.

Files overwrite existing files or copy into an existing directory using the
source basename. A directory copied to a missing destination creates that
directory; copied to an existing directory, it creates/merges a child using the
source basename. A source ending in `/.` copies only its contents. Destination
parents must already exist. A file destination ending in `/` requires an existing
directory, and file/directory type conflicts fail. Unrelated files are preserved.

Unlike diff and commit, copies include declared volumes and bind mounts. The
saved mount sources define the logical tree, including before first start and
while junctions are detached. Mounted content masks private files at the same
path. Copying never attaches/detaches mounts, creates missing backing sources, or
changes grants. Other junctions, symlinks, and reparse points are rejected.

`-` as the host endpoint means a **plain tar stream**, not raw file contents.
Output includes the selected basename; a root or `/.` selection emits relative
contents. Tar input extracts into an existing container directory, creates
parents for archive entries, and merges existing contents. Standard tar metadata
for long/Unicode paths is supported. Archives are staged on disk and fully
validated before destination changes. Links, special files, unsafe paths,
case-insensitive duplicate entries, conflicting entry types, truncated input, and
nonzero trailing data are rejected. Zero block padding is allowed. Compressed tar
input is unsupported. For binary-safe redirection from PowerShell:

```powershell
cmd /c "quasidock cp worker:/workspace - > workspace.tar"
cmd /c "quasidock cp - worker:/workspace < workspace.tar"
```

Created, running, paused, and exited containers are supported. Copy does not
pause processes, coordinate shared-volume writers, or snapshot live registry
hives. Live changes, disappearing files, exclusive file locks, or permissions can
fail a copy. Source size/modification-time changes detected during a file copy
fail that file; this is not a consistent snapshot of a running application.

Modification times are preserved; new files inherit destination permissions.
Owners, ACLs, security labels, alternate streams, and Windows-specific attributes
are not copied. Each file is published through a sibling temporary file and
rename, so a failed write does not replace its destination with partial contents
or write through an existing hardlink. Same-file copies and overlapping directory
trees, including aliases through mounts, are rejected. Completed files and
directories can remain after later I/O failures; there is no whole-copy rollback.
Failed tar output may be partial and must be discarded.

Success returns 0, failure returns 1, and Ctrl+C returns 130 after cleanup.
Ordinary copies print nothing; tar output contains only archive bytes. `--help`
and `-h` show usage. Ownership/link and progress options (`-a`, `-L`, `-q`) are
unsupported. Container, image, and supervisor formats require no migration.

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
