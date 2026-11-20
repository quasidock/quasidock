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
