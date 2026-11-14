# Quasidock

Quasidock runs sandboxed Windows programs from OCI-distributed images using a
Docker-style CLI. It is a Go fork of [Zigsaw](https://github.com/zigsaw-io/zigsaw),
focused on image distribution and native Windows execution.

Version **0.6.0** supports Docker-style container lifecycle management (`run`, `ps`,
`logs`, `stop`, `rm`, `inspect`, `prune`), per-container isolated data, image
management, and Quasidock images for `windows/amd64`. Dockerfiles, the Docker
Engine API, bind mounts, and general Docker images are not supported.

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
`ghcr.io/zigsaw-io`; `QUASIDOCK_REGISTRY` changes it. An omitted remote tag is
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
quasidock stop node-demo
quasidock rm node-demo
quasidock run --entrypoint=node.exe node -e 'console.log(process.version)'
quasidock run --pull=never -e EXAMPLE=value node node.exe -e 'console.log(process.env.EXAMPLE)'
```

Run options stop at IMAGE; subsequent flags belong to the application. Single-letter
boolean flags before IMAGE can be combined (for example `-it`, `-dit`).

| Option | Behavior |
|---|---|
| `--name NAME` | Assign a unique container name (`[a-zA-Z0-9][a-zA-Z0-9_.-]*`) |
| `--rm` | Delete the container, its `data/`, and its HKCU hive on normal or signaled exit |
| `-d`, `--detach` | Supervise the container in the background and print its 64-hex container ID |
| `-i`, `--interactive` | Attach host stdin when running in the foreground (otherwise stdin is `\\.\NUL`) |
| `-t`, `--tty` | Record TTY preference on container metadata (pipe-backed streams; no ConPTY yet) |
| `--pull=missing` | Default: use the cached selection, or pull if missing |
| `--pull=always` | Pull before running; a failed pull fails the run |
| `--pull=never` | Use only local images; a missing image is an error |
| `-e NAME=VALUE`, `--env NAME=VALUE` | Set a variable; the last setting wins |
| `-e NAME`, `--env NAME` | Inherit a host variable, or unset it if absent |
| `--verbose` | Print runtime diagnostics |

Each `run` allocates an isolated container under `containers/<id>/` with its own
`state.json`, `container.log`, and `data/` directory (`${data}`, `HOME`, `APPDATA`,
`LOCALAPPDATA`, `TEMP`, cache, and per-container HKCU hive). Multiple containers—
including different versions of the same app—can run concurrently without sharing
state. Containers persist after exit unless `--rm` is set. Bind mounts (`-v`,
`--mount`) and Docker-style `-p` port publishing are deferred.

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

## Upgrading

### From 0.5 to 0.6

- `--ephemeral` is removed; use `--rm` (passing `--ephemeral` now errors and points
  to `--rm`).
- Mutable app state moved from shared `data/<app-id>` to per-container
  `containers/<id>/data`. Legacy `data/<app-id>` directories remain untouched on
  disk until collected with `quasidock prune --data`.
- Foreground and detached runs both persist exited container state, logs, and data
  until removed with `--rm`, `rm`, `container prune`, or `prune`. Exited containers
  pin their image against `rmi`.
- `rm` now operates on containers (use `rmi` or `image rm` for images). Top-level
  `inspect` resolves containers first and falls back to images; use `image inspect`
  or `container inspect` to disambiguate.

### From 0.4 to 0.5

Finish existing runs before first opening the store with 0.5+. The one-time
migration creates `images/index.json`, retaining legacy refs, blobs, deployments,
and data. Retained refs are never imported again. Use 0.5+ for the migrated store;
older binaries do not read catalog changes.

| Previous syntax (0.4) | Replacement |
|---|---|
| `list` | `images`, or `image inspect -f TEMPLATE IMAGE` for scripts |
| `rm [--delete-data] APP` | `rmi IMAGE`; collect orphan data with `prune --data` |
| `info IMAGE` | `image info IMAGE` |
| `ls IMAGE` | `image files IMAGE` |
| `cat IMAGE PATH` | `image cat IMAGE PATH` |
| `push SOURCE TARGET` | `tag SOURCE TARGET`, then `push TARGET` |
| `run --command=NAME APP ARGS` | `run --entrypoint=NAME IMAGE ARGS` |
| `run APP ARGS` | Specify `COMMAND` after IMAGE, or select an entrypoint |
| `-v` for verbosity | `--verbose` |

Data lives in `%QUASIDOCK_HOME%`, or `%LOCALAPPDATA%\quasidock` by default.

### Recipe and image schema (Docker-style)

Quasidock uses Docker-style short image IDs (`node`, `busybox`, `gtk4`, etc.),
required `entrypoint` and optional default `cmd` fields (instead of `command`,
`args`, and `exports`), and `from` base-image aliases (instead of `runtimes`).
Legacy Zigsaw recipes and published Zigsaw image configs without `entrypoint`
are no longer compatible and fail config validation. See
[docs/recipes.md](docs/recipes.md) for the current schema.
