# Quasidock

Quasidock runs sandboxed Windows programs from OCI-distributed images using a
Docker-style CLI. It is a Go fork of [Zigsaw](https://github.com/zigsaw-io/zigsaw),
focused on image distribution and native Windows execution.

Version **0.5.0** supports image management and foreground runs of Quasidock
images, including compatible published Zigsaw images, for `windows/amd64`.
Dockerfiles, the Docker Engine API, and general Docker images are not supported.

## Build and try it

On Windows, with the Go toolchain specified by `go.mod`:

```powershell
go run ./scripts/build.go
.\out\bin\quasidock.exe pull org.nodejs.node
.\out\bin\quasidock.exe run org.nodejs.node node.exe --version
.\out\bin\quasidock.exe images
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
| `image inspect` | `inspect` | Local JSON metadata, or `-f TEMPLATE` Go templates |
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
quasidock inspect -f '{{.Id}}' org.nodejs.node
quasidock inspect -f '{{.Quasidock.Version}}' org.nodejs.node
quasidock tag org.nodejs.node ghcr.io/example/node:demo
quasidock push ghcr.io/example/node:demo
```

Image IDs are **manifest digests**. Inspect includes `Id`, `RepoTags`,
`RepoDigests`, `Os`, `Architecture`, `Size`, `Config`, and the original app
configuration under `Quasidock`. `Source`, `Default`, and optional `Lazy` describe
local provenance and state. Size counts stored layers and config bytes, counting
each shared layer once within an image.

`rmi repository:tag` untags that reference. `rmi sha256:...` removes the manifest
and all its local names; a bare app ID removes its current default image.
Running images cannot be removed. Shared layers remain while other images or
builds need them. App data stays until the app has no cached images and you run
`quasidock prune --data`.

## Foreground runs

```text
quasidock run [OPTIONS] IMAGE [COMMAND] [ARG...]
```

Without a command, the image's `command` and `args` provide its default CMD.
An explicit command replaces both. `--entrypoint` selects an executable or an
exported command, clears the default CMD, and receives the arguments after IMAGE.
Exported commands retain their own configured argument prefixes.

```powershell
quasidock run org.nodejs.node node.exe -e 'console.log(process.version)'
quasidock run --entrypoint=node org.nodejs.node -e 'console.log(process.version)'
quasidock run --pull=never org.nodejs.node node.exe --version
quasidock run -e EXAMPLE=value org.nodejs.node node.exe -e 'console.log(process.env.EXAMPLE)'
```

Run options stop at IMAGE; subsequent flags belong to the application.

| Option | Behavior |
|---|---|
| `--pull=missing` | Default: use the cached selection, or pull if missing |
| `--pull=always` | Pull before running; a failed pull fails the run |
| `--pull=never` | Use only local images; a missing image is an error |
| `-e NAME=VALUE`, `--env NAME=VALUE` | Set a variable; the last setting wins |
| `-e NAME`, `--env NAME` | Inherit a host variable, or unset it if absent |
| `--ephemeral` | Use fresh app data and delete it after the run |
| `--verbose` | Print runtime diagnostics |

Versions share data keyed by app ID. Concurrent runs of the same manifest are
allowed; another version must wait until existing runs finish. Foreground runs
inherit terminal streams and return the child's exit code. Named containers,
detached runs, `-it`, `--rm`, mounts, and Docker-style port publishing are deferred.
`-v` is reserved for future volume support.

## Windows sandbox and Quasidock extensions

`--sandbox=soft|low|appcontainer` chooses environment shaping, low-integrity
execution, or AppContainer execution. The image chooses the default, falling
back to `low`. The Windows permission model retains `--filesystem`,
`--nofilesystem`, `--share`, `--unshare`, and service-name-based `--port` options.

Recipe builds remain available through `build [recipe.json|directory]`.
Use `image info IMAGE` for remote metadata, `image files IMAGE` for a remote
eStargz file listing, and `image cat IMAGE PATH` to read a file. Search, login,
trust, signatures, grants, repair, and maintenance pruning remain available.
Run `quasidock help` for supported commands.

Pull reports signature status and permissions. With a matching trust key, missing
signatures warn and invalid signatures are rejected. Without a matching key,
images are reported as unchecked. Explicit and automatic pull use the same path.

## Upgrading from 0.4

Finish existing runs before first opening the store with 0.5. The one-time
migration creates `images/index.json`, retaining legacy refs, blobs, deployments,
and data. Retained refs are never imported again. Use 0.5 for the migrated store;
older binaries do not read catalog changes.

| Previous syntax | Replacement |
|---|---|
| `list` | `images`, or `inspect -f TEMPLATE IMAGE` for scripts |
| `rm [--delete-data] APP` | `rmi IMAGE`; collect orphan data with `prune --data` |
| `info IMAGE` | `image info IMAGE` |
| `ls IMAGE` | `image files IMAGE` |
| `cat IMAGE PATH` | `image cat IMAGE PATH` |
| `push SOURCE TARGET` | `tag SOURCE TARGET`, then `push TARGET` |
| `run --command=NAME APP ARGS` | `run --entrypoint=NAME IMAGE ARGS` |
| `run APP ARGS` | Specify `COMMAND` after IMAGE, or select an entrypoint |
| `-v` for verbosity | `--verbose` |

Data lives in `%QUASIDOCK_HOME%`, or `%LOCALAPPDATA%\quasidock` by default.
No recipe or published-image schema migration is required.
