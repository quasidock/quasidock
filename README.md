# Quasidock

Quasidock runs sandboxed Windows programs from OCI-distributed images using a
Docker-style CLI.

Version **0.10** supports Docker-style container lifecycle management (`run`, `ps`,
`logs`, `exec`, `start`, `restart`, `kill`, `stop`, `rm`, `inspect`, `prune`), per-container isolated data, image
management, named volumes, local directory bind mounts, and Quasidock images for
`windows/amd64`. Supported Node.js, Go, Python, and Ubuntu/Debian Dockerfiles, including
multi-stage builds with `COPY --from`, can be
[converted to native recipes](docs/dockerfile-conversion.md). The Docker Engine
API and general Docker images are not supported.

## Build and try it

On Windows, with the Go toolchain specified by `go.mod`:

```powershell
go run ./scripts/build.go
.\out\bin\quasidock.exe pull node
.\out\bin\quasidock.exe run --rm node node.exe --version
.\out\bin\quasidock.exe images
.\out\bin\quasidock.exe ps -a
```

Builds produce `out/bin/quasidock.exe`, `quasidock-relay.exe`, and
`quasidock-host.exe`. Pass `--strip` to build with `-trimpath -ldflags="-s -w"`.

```powershell
go test -count=1 ./...
```

Tests run as the current Windows user; some create temporary AppContainer profiles
and HKCU keys. CLI integration tests use a loopback registry and locally built
executables, without downloaded applications.

## Images

| Command | Alias | Purpose |
|---|---|---|
| `image ls` | `images` | List references; `-q` prints unique full IDs |
| `image inspect` | — | Local image JSON metadata, or `-f TEMPLATE` Go templates (`inspect` resolves containers first, then images) |
| `image pull` | `pull` | Download and cache an image without prompting |
| `image tag` | `tag` | Add or move a local name |
| `image push` | `push` | Publish the exact locally selected image |
| `image rm` | `rmi` | Untag a reference or remove a cached manifest |

References are `registry/repository[:tag][@sha256:...]`, or
`app-id[:tag][@digest]` in the default registry. The default is
`ghcr.io/quasidock/quasidock`; `QUASIDOCK_REGISTRY` changes it. An omitted remote tag is
`latest`. Full `sha256:...` IDs select cached manifests locally.

Multiple versions coexist. A bare cached app ID selects its last successful
build or pull. Explicit references select their own image. Moving a tag retains
the previous manifest and does not change the app's default.

```powershell
quasidock inspect -f '{{.Id}}' node
quasidock inspect -f '{{.Quasidock.Version}}' node
quasidock tag node ghcr.io/example/node:demo
quasidock push ghcr.io/example/node:demo
```

Image IDs are **manifest digests**. Inspect includes `Id`, `RepoTags`,
`RepoDigests`, `Os`, `Architecture`, `Size`, `Config`, and the original app
configuration under `Quasidock`. `Source`, `Default`, and optional `Lazy` describe
local provenance and state. Size counts stored layers and config bytes, counting
each shared layer once within an image.

`rmi repository:tag` untags that reference. `rmi sha256:...` removes the manifest
and all its local names; a bare app ID removes its current default image.
Images referenced by running or exited containers cannot be removed (untagging a
secondary alias succeeds; removing the final reference requires removing those
containers first). Shared layers remain while other images, containers, or
builds need them. `quasidock prune` collects stopped containers first, then
newly unpinned unnamed images and unused deployments; `prune --data` also
removes leftover legacy app data once unused.

## Containers and running images

| Command | Alias | Purpose |
|---|---|---|
| `container ls` | `ps` | List running containers (`-a` includes exited; `-q` prints IDs) |
| `container inspect` | `inspect` | Container JSON metadata or `-f TEMPLATE` (`inspect` falls back to images) |
| `container logs` | `logs` | Fetch captured output (`-f`/`--follow`, `-n`/`--tail LINES`) |
| `container exec` | `exec` | Run another command in a running container (`-d`, `-i`, `-t`, `-e`, `-w`) |
| `container start` | `start` | Start existing containers in the background |
| `container restart` | `restart` | Stop and start containers (`-t`/`--time SECONDS`, default 10s) |
| `container kill` | `kill` | Immediately terminate process trees; `-s`/`--signal TERM` requests graceful shutdown |
| `container stop` | `stop` | Stop running containers (`-t`/`--time SECONDS`, default 10s) |
| `container rm` | `rm` | Remove containers (`-f`/`--force` stops running containers first) |
| `container prune` | — | Remove all stopped containers |

```text
quasidock run [OPTIONS] IMAGE [COMMAND] [ARG...]
```

Without a command or `--entrypoint`, the container runs the image's `entrypoint`
followed by its default `cmd`. Passing a `COMMAND` after `IMAGE` resolves that
executable in the image and runs `COMMAND [ARG...]`, ignoring `entrypoint` and
`cmd` — unless `COMMAND` matches the image's `id` (or the entrypoint executable's
stem when `entrypoint` has leading arguments), in which case it keeps
`entrypoint` and replaces `cmd` with `[ARG...]`. `--entrypoint` overrides the
entrypoint (or keeps the configured `entrypoint` when set to the image's `id`),
clears default `cmd`, and passes the remaining arguments after `IMAGE`.

```powershell
quasidock run --rm node node.exe -e 'console.log(process.version)'
quasidock run -d --name node-demo node node.exe -e 'setInterval(() => {}, 1000)'
quasidock logs node-demo
quasidock exec node-demo node.exe --version
quasidock exec -i -e EXAMPLE=value -w '${data}' node-demo node.exe -e 'console.log(process.env.EXAMPLE)'
quasidock stop node-demo
quasidock start node-demo
quasidock restart -t 5 node-demo
quasidock kill node-demo
quasidock rm node-demo
quasidock run --entrypoint=node.exe node -e 'console.log(process.version)'
quasidock run --pull=never -e EXAMPLE=value node node.exe -e 'console.log(process.env.EXAMPLE)'
```

Run options stop at IMAGE; subsequent flags belong to the application. Single-letter
boolean flags before IMAGE can be combined (for example `-it`, `-dit`).

| Option | Behavior |
|---|---|
| `--name NAME` | Assign a unique container name (`[a-zA-Z0-9][a-zA-Z0-9_.-]*`) |
| `--rm` | Delete the container and its private data on exit; preserve mounted contents and mounted registry state |
| `-d`, `--detach` | Supervise the container in the background and print its 64-hex container ID |
| `-i`, `--interactive` | Attach host stdin when running in the foreground (otherwise stdin is `\\.\NUL`) |
| `-t`, `--tty` | Record TTY preference on container metadata (pipe-backed streams; no ConPTY yet) |
| `--pull=missing` | Default: use the cached selection, or pull if missing |
| `--pull=always` | Pull before running; a failed pull fails the run |
| `--pull=never` | Use only local images; a missing image is an error |
| `-e NAME=VALUE`, `--env NAME=VALUE` | Set a variable; the last setting wins |
| `-e NAME`, `--env NAME` | Inherit a host variable, or unset it if absent |
| `-v SOURCE:TARGET[:rw]`, `--volume SOURCE:TARGET[:rw]` | Mount a named volume or host directory at `${data}` or a data subpath |
| `--mount type=volume\|bind,source=SOURCE,target=TARGET` | Explicitly select a writable directory mount |
| `--verbose` | Print runtime diagnostics |

Each `run` allocates an isolated container under `containers/<id>/` with its own
`state.json`, `container.log`, and `data/` directory (`${data}`, `HOME`, `APPDATA`,
`LOCALAPPDATA`, `TEMP`, cache, and per-container HKCU hive). Multiple containers—
including different versions of the same app—can run concurrently without sharing
state unless mounted storage is explicitly shared. Containers persist after exit
unless `--rm` is set. Docker-style `-p` port publishing remains deferred.

`start` and `restart` reuse the same container ID, saved command, image digest,
working directory, private data, mounts, and registry state. They append to the
existing log and never pull a newer image. `start` succeeds without another launch
when a container is already running; `restart` also starts stopped containers.
Both launch in the background; attached start is unsupported. A failed start keeps
the existing record and data for retry. Restart preserves a running `--rm`
container through the stop/start cycle, then automatic removal resumes on its
next exit. It also retains the record if the replacement launch fails.

`kill` defaults to `KILL` and waits for process-tree and runtime cleanup. `TERM`
sends Ctrl+Break to a detached container's console process group and returns after
Windows accepts the request; applications may ignore it. TERM has no timeout or
automatic escalation. Signals also accept `SIGKILL`/`9` and `SIGTERM`/`15`,
case-insensitively. Foreground TERM is unsupported so native terminal Ctrl+C keeps
its existing behavior; KILL works for foreground containers. `stop` and `restart`
wait their requested grace period before forcing termination; `-t 0` forces it
immediately. Killing a stopped container is an error.

`exec [OPTIONS] CONTAINER COMMAND [ARG...]` (also `container exec`) runs an
explicit command in a running container's existing sandbox, data, mounts, and
registry session. Options stop at CONTAINER; later flags belong to COMMAND.
Commands use the effective working directory, image deployment, and exec `PATH`
for executable lookup, without substituting the image's entrypoint or default
command. Shell expressions require an explicit shell such as `cmd.exe /c`.

Exec inherits the environment and working directory from container startup,
including saved `run -e` values. Repeatable `-e` / `--env NAME[=VALUE]` overrides
only this command; a bare name inherits the exec caller's value or removes the
variable if absent. `-w` / `--workdir DIR` selects an existing directory, supports
placeholders such as `${data}`, and resolves relative directories against the
container's initial working directory. It does not add filesystem permissions.

Foreground exec returns the command's exit code and sends stdout/stderr to its
caller; `-i` forwards stdin and EOF (otherwise stdin is NUL). `-t` accepts a
pipe-backed TTY preference without allocating ConPTY. `-d` returns silently after
successful launch, discards output, and uses NUL stdin even with `-i`. Exec output
does not appear in `logs`. Ctrl+C returns 130 and cancels that exec process tree;
disconnecting the foreground caller also cancels it. Finishing an exec command
ends its remaining descendants. All exec processes end when the main command
exits or the container stops, restarts, or is removed. Older running supervisors
must be stopped and started with the new binary before they support exec.

Values inherited with `run -e NAME`, including absent variables, are saved when
the container is created and reused across starts. Existing stopped records from
older binaries capture missing snapshots on their first start. Finish active
containers before replacing the binary: live supervisors from older binaries use
a different control protocol.

## Persistent storage

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

## Windows sandbox and Quasidock extensions

`--sandbox=soft|low|appcontainer` chooses environment shaping, low-integrity
execution, or AppContainer execution. The image chooses the default, falling
back to `low`. The Windows permission model retains `--filesystem`,
`--nofilesystem`, `--share`, `--unshare`, and service-name-based `--port` options.

Use `image info IMAGE` for remote metadata, `image files IMAGE` for a remote
eStargz file listing, and `image cat IMAGE PATH` to read a file. Search, login,
trust, signatures, grants, repair, and maintenance pruning remain available.
Run `quasidock help` for supported commands.

Pull reports signature status and permissions. With a matching trust key, missing
signatures warn and invalid signatures are rejected. Without a matching key,
images are reported as unchecked. Explicit and automatic pull use the same path.

### Recipes

Recipe builds remain available through `build [recipe.json|directory]` (using
`quasidock.json` when given a directory or no arguments). See
[docs/recipes.md](docs/recipes.md) for the Docker-style recipe schema,
placeholders, vendoring, and pinned dependencies.

Convert a supported Linux Dockerfile before building:

```powershell
quasidock convert . --id my-app
quasidock build .
quasidock run --rm my-app
```

Conversion writes a reviewable `quasidock.json`, pins Windows tool images, and
keeps project sources live. Unsupported instructions produce file-and-line
diagnostics without writing a partial recipe. For multi-stage input, select a
named stage with `--target NAME` and replace individual native bases with
`--stage-base NAME_OR_INDEX=IMAGE`. See the
[conversion compatibility table](docs/dockerfile-conversion.md).

Ubuntu/Debian conversion maps a bounded apt package catalog to native Python,
Node.js, Go, Git, Make, CMake/Ninja, and Clang images. Use `--tool TOOL=IMAGE` or
`--stage-tool NAME_OR_INDEX:TOOL=IMAGE` to select native versions. Distro tags do
not select runtime versions; conversion reports substitutions and pins native
dependencies by digest.

### Recipe and image schema (Docker-style)

Quasidock uses Docker-style short image IDs (`node`, `busybox`, `gtk4`, etc.),
required `entrypoint` and optional default `cmd` fields (instead of `command`,
`args`, and `exports`), and `from` base-image aliases (instead of `runtimes`).
See [docs/recipes.md](docs/recipes.md) for the current schema.
