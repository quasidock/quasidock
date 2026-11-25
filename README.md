# Quasidock

Quasidock runs sandboxed Windows programs from OCI-distributed images using a
Docker-style CLI.

Currently it supports Docker-style container lifecycle management (`create`, `run`, `ps`,
`logs`, `stats`, `exec`, `attach`, `start`, `restart`, `pause`, `unpause`, `kill`, `stop`, `rm`, `inspect`, `diff`, `commit`, `prune`), per-container isolated data, image
management, named volumes, local directory bind mounts, and Quasidock images for
`windows/amd64`.

Supported Node.js, Go, Python, PHP CLI, Composer, and Ubuntu/Debian Dockerfiles, including
multi-stage builds with `COPY --from`, can be
[converted to native recipes](docs/dockerfile-conversion.md). The Docker Engine
API and general Docker images are not supported.

## Build and try it

On Windows, select the Go toolchain specified by `go.mod` so image digests
match CI. Go versions can produce different compressed bytes for the same files.

```powershell
$env:GOTOOLCHAIN = "go$(go list -m -f '{{.GoVersion}}')"
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

With [Task](https://taskfile.dev) installed, the root `Taskfile.yml` provides
shortcuts for common development commands:

```powershell
task                      # List available tasks
task build                # Build all three executables
task build:strip          # Build with trimming and stripped debug information
task run -- help          # Build and run Quasidock with the supplied arguments
task test                 # Run all Go tests without cached results
task fmt                  # Format Go source files
task vet                  # Run Go static analysis
task lint:build           # Build the golangci-lint image from source
task lint                 # Lint Go code and save out/golangci-lint.json
task check                # Run vet, then tests
```

Pass extra build options or test flags after `--`, for example
`task build -- --prefix out/custom` or `task test -- -run TestName`.

`task lint` builds the pinned golangci-lint image, then runs its bundled Go
toolchain against the root module, including tests. The first run downloads
dependencies; the `quasidock-lint-cache` named volume preserves module, build,
and analysis caches across runs. It enables the standard linters plus `gosec`,
`bodyclose`, and `errorlint`, prints findings,
and writes `out/golangci-lint.json`. Findings return a nonzero exit status for
review; lint is separate from `task check`. Pass linter options after `--`, for
example `task lint -- --verbose`.

Without Task, use:

```powershell
go run ./scripts/build.go
.\out\bin\quasidock.exe build recipes/golangci-lint.json
.\out\bin\quasidock.exe run --rm --pull=never --entrypoint=golangci-lint.exe -v 'quasidock-lint-cache:${data}/cache' golangci-lint run --config .golangci.yml ./...
```

## Images

| Command | Alias | Purpose |
|---|---|---|
| `image ls` | `images` | List references; `-q` prints unique full IDs |
| `image inspect` | — | Local image JSON metadata, or `-f TEMPLATE` Go templates (`inspect` resolves containers first, then images) |
| `image pull` | `pull` | Download and cache an image without prompting |
| `image tag` | `tag` | Add or move a local name |
| `image push` | `push` | Publish the exact locally selected image |
| `image save` | `save` | Save local images and all their aliases to an OCI tar archive |
| `image load` | `load` | Load images from an OCI tar archive; `--force` replaces conflicting names |
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
Images referenced by created, running, paused, or exited containers cannot be removed (untagging a
secondary alias succeeds; removing the final reference requires removing those
containers first). Shared layers remain while other images, containers, or
builds need them. `quasidock prune` collects stopped containers first, then
newly unpinned unnamed images and unused deployments; `prune --data` also
removes leftover legacy app data once unused.

### Offline image transfer

`save` (also `image save`) writes selected local images and **all their local
aliases** to an OCI tar archive. It preserves image IDs and original compressed
layer bytes, sharing each blob once. A bare app ID selects its current default,
as with other local image commands; it does not select every cached version.

```powershell
quasidock save -o tools.tar node go
quasidock load -i tools.tar
quasidock run --rm --pull=never node node.exe --version
```

`load` (also `image load`) accepts plain tar or gzip-compressed tar, verifies
the complete archive, and restores images and names together. Conflicting names
fail the entire import unless `--force` is supplied. Existing app defaults and
container image selections are preserved, including with `--force`. For apps
without a default, load restores the archived default when included, otherwise
choosing the lexicographically smallest imported manifest digest. Repeated loads
are safe; `-q` / `--quiet` suppresses the loaded-image list.

Both commands work offline. Incomplete lazy images must be fully pulled by their
exact repository digest before saving. Save writes binary tar to stdout by
default, and load reads stdin; use `-o` / `--output` and `-i` / `--input` to avoid
shell text conversion. File saves replace existing output only after success.

Container records, live private data, and volumes are separate from images.
Capture private data and declared HKCU state with `commit` before transferring:

```powershell
quasidock commit worker example.test/worker:saved
quasidock save -o worker.tar example.test/worker:saved
quasidock load -i worker.tar
```

See [archive format and limits](docs/image-archives.md) for the OCI layout,
metadata, and compatibility details.

## Containers and running images

| Command | Alias | Purpose |
|---|---|---|
| `container create` | `create` | Save a new container without starting it; print its full ID |
| `container ls` | `ps` | List containers (`-a`, `-q`, `--filter`, `--last`, `--size`, `--no-trunc`) |
| `container stats` | `stats` | Stream CPU, private committed memory, cumulative I/O, and process counts |
| `container inspect` | `inspect` | Container JSON metadata or `-f TEMPLATE` (`inspect` falls back to images) |
| `container diff` | `diff` | Compare private `${data}` with image data; exclude mounted storage |
| `container cp` | `cp` | Copy files/folders between container data and the host; `-` streams tar |
| `container commit` | `commit` | Save private data and launch configuration as a new image |
| `container logs` | `logs` | Fetch captured output (`-f`/`--follow`, `-n`/`--tail LINES`) |
| `container exec` | `exec` | Run another command in a running container (`-d`, `-i`, `-t`, `-e`, `-w`) |
| `container attach` | `attach` | Attach to a background container's live output and optional stdin |
| `container start` | `start` | Start existing containers; `-a` attaches output, `-i` also attaches saved stdin |
| `container restart` | `restart` | Stop and start containers (`-t`/`--time SECONDS`, default 10s) |
| `container pause` | `pause` | Freeze all processes in running containers |
| `container unpause` | `unpause` | Resume paused containers without restarting them |
| `container kill` | `kill` | Immediately terminate process trees; `-s`/`--signal TERM` requests graceful shutdown |
| `container stop` | `stop` | Stop running containers (`-t`/`--time SECONDS`, default 10s) |
| `container rm` | `rm` | Remove containers (`-f`/`--force` stops running containers first) |
| `container prune` | — | Remove all stopped containers |
| `system events` | `events` | Follow container events; replay retained history with time filters |

```text
quasidock ps [OPTIONS]
quasidock create [OPTIONS] IMAGE [COMMAND] [ARG...]
quasidock run [OPTIONS] IMAGE [COMMAND] [ARG...]
```

`ps` (also `container ls` and `container list`) shows running and paused
containers, newest first. `-a` / `--all` includes created and exited containers;
`-q` / `--quiet` prints IDs only. `--no-trunc` shows full IDs, including with
`-q`, and full recorded executable paths and arguments.

`stats` (also `container stats`) samples running and paused containers every
second. Supply names or ID prefixes to select containers; `-a` / `--all`
includes created and exited containers. Explicit stopped containers are also
shown, with zero metrics. `--no-stream` collects two samples one second apart,
prints one result, and exits. `--no-trunc` shows full IDs.

```powershell
quasidock stats
quasidock stats --no-stream worker
quasidock container stats --all --no-stream --format json
quasidock stats --format '{{.Name}}: {{.CPUPerc}} {{.MemUsage}}'
```

The columns are `CONTAINER ID`, `NAME`, `CPU %`, `PRIVATE MEMORY`, `I/O`, and
`PIDS`. CPU uses whole-host capacity: one busy logical processor on an
eight-processor host is about 12.5%. Memory counts private committed bytes,
including paged-out memory. I/O shows cumulative read/write bytes for the
current run, including exited child processes; it measures Windows process
I/O rather than physical block-device traffic. PIDS counts active processes,
including `exec` children and the container's relay. Shared host services and
the supervisor are excluded.

`--format` accepts `table` (the default), `table TEMPLATE`, `json` (JSON Lines),
or a Go template. Fields are `.Container`, `.ID`, `.Name`, `.CPUPerc`,
`.MemUsage`, `.IO`, and `.PIDs`; `{{json .}}` also emits JSON. Values are
formatted strings, using the same units as the table. `.Container` preserves
the supplied selector, or uses the displayed ID when no selectors were given.
Tables refresh in place on a console; redirected output and bare templates
append each sample without terminal escapes.

Stats follows restarts, resetting its counters and CPU baseline. Newly
discovered runs show `--` for CPU until a second sample is available. Without
selectors, stats keeps discovering new containers even when none are running;
with selectors, it finishes when all selected containers have been removed.
Ctrl+C exits with code 130 and leaves workloads running. Existing supervisors
without `State.StatsProtocol` must be stopped and started with the current
binary before stats is available. See [stats limits](docs/gaps.md#container-stats).

`events` (also `system events`) follows new container events. `--since` first
replays matching history, then follows; each store retains its latest **256**
events across CLI invocations. `--until` ends the stream at a deadline, or
queries history and exits when that deadline is in the past. Ctrl+C exits with
code 130. Use two terminals to watch events while running container commands:

```powershell
quasidock events
quasidock events --since 1h --until 0s --filter event=die --format json
quasidock events --since 10m --filter container=worker --format '{{.Action}} {{.Actor.ID}}'
```

Times accept Unix seconds (with optional fractional seconds), RFC3339 timestamps,
local dates or date-times, and Go durations such as `1h30m`. Positive durations
refer to the past, relative to command startup; offset-free dates use the local
timezone. `--since` is inclusive and `--until` is exclusive. Without `--since`,
only events after subscription are shown, including when `--until` is supplied.

Repeatable `-f` / `--filter KEY=VALUE` accepts `event` (exact action), `container`
(case-insensitive exact name, full ID, or hex prefix of at least two characters),
`image` (exact recorded reference or manifest digest), and `type=container`.
Same-key values use OR; different keys use AND. Filters also match removed
containers. Default output includes a UTC timestamp, full container ID, and
sorted attributes. `--format json` or `--format '{{json .}}'` prints JSON Lines;
other `--format` values are Go templates.

Recorded actions are `create`, `start`, `die`, `destroy`, `pause`, `unpause`,
`kill`, `stop`, `restart`, `exec_create`, `exec_start`, and `exec_die`. Exit events
include `exitCode`; kill events include signal `9` or `15`; exec events share an
`execID`. Idempotent start/stop commands add no events. See
[event recording limits](docs/gaps.md#container-events) for retention, crash
recovery, and compatibility details.

`-f` / `--filter KEY=VALUE` supports `id` (full ID or prefix), `name`
(case-sensitive regular expression, matching with or without the leading `/`),
`status` (`created`, `running`, `paused`, or `exited`), `exited` (integer exit
code), and `before` / `since` (a container name, full ID, or unique ID prefix).
Repeated values for the same key use OR; different keys use AND. Reference
containers are excluded from their own `before` / `since` range. A `status`
filter includes stopped containers without `-a`; `exited` alone requires `-a`
or a positive `--last` and matches only containers that actually started.

`-n` / `--last N` returns the newest N matches, including all states, after
filtering. Zero and negative values leave the results unlimited and retain the
normal visibility rules. `-s` / `--size` adds a `SIZE` column counting current
private-data file bytes, including copied image seed files. Mounted storage,
reparse points, logs, metadata, and shared image files are excluded. There is
no virtual image total. Quiet mode skips size scans. Live applications may
change files during a scan; a scan error fails the command before printing rows.

```text
quasidock ps --filter status=exited --last 5
quasidock ps -a --filter "name=^web-" --filter exited=0 --size
quasidock ps -aq --no-trunc
```

`create` saves the image digest and launch configuration, allocates private data,
and prints the full container ID without starting the program. It supports the
same configuration options as `run`, except `-d` / `--detach`. Created containers
appear as `Created` in `ps -a`; their logs are empty until the first start.
`-i` and `-t` save preferences. Plain `start` launches in the background;
`start -a` streams output and `start -ai` also forwards stdin if `-i` was saved.
`--rm` keeps an unstarted container and a failed-start record,
then removes it after a successful launch exits. `rm` and `container prune` can
remove created containers immediately.

`pause CONTAINER [CONTAINER...]` freezes the container's process tree, including
existing `exec` commands and its relay, for foreground or background runs.
`unpause` resumes the same processes, retaining their PIDs and startup timestamps.
Paused containers remain in `ps` as `Up ... (Paused)`; inspect reports
`State.Status="paused"`, `State.Running=true`, and `State.Paused=true`.
Logs and inspect remain available, and paused containers retain their data,
mounts, and image references. Prune skips them.

New `exec`, `attach`, and attached `start -a`/`-i` requests fail while paused;
existing sessions stay connected and continue after unpause. Plain `start`
leaves a paused container paused. `stop` and `restart` unpause before their
grace period, and detached `kill --signal TERM` unpauses before signaling.
Foreground TERM remains unsupported and leaves the container paused. KILL,
zero-timeout stop/restart, and `rm -f` terminate frozen processes directly.
Repeated pause or unpause requests report a state error, and neither command
accepts stopped containers. Supervisors started by older binaries must be
stopped and started with the current binary before pause is available.

Creation validates options, image metadata, and mount sources. Executable lookup
and runtime-dependent checks happen at start. Named volumes are created or
referenced at creation, while mount attachment, sandbox setup, registry sessions,
and service allocation wait until start.

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
quasidock create --name node-later node node.exe -e 'console.log(process.version)'
quasidock start node-later
quasidock logs node-later
quasidock rm node-later
quasidock create -it --name node-repl node node.exe -i
quasidock start -ai node-repl
# Detach with Ctrl+P, Ctrl+Q, then reattach:
quasidock attach node-repl
quasidock run -d --name node-demo node node.exe -e 'setInterval(() => {}, 1000)'
quasidock logs node-demo
quasidock pause node-demo
quasidock inspect -f '{{.State.Paused}}' node-demo
quasidock unpause node-demo
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

Run and create options stop at IMAGE; subsequent flags belong to the application.
Single-letter boolean flags before IMAGE can be combined (for example `-it`, or
`-dit` with `run`).

| Option | Behavior |
|---|---|
| `--name NAME` | Assign a unique container name (`[a-zA-Z0-9][a-zA-Z0-9_.-]*`) |
| `--rm` | Delete the container and its private data on exit; preserve mounted contents and mounted registry state |
| `-d`, `--detach` | `run` only: supervise the container in the background and print its 64-hex container ID |
| `-i`, `--interactive` | Enable stdin: forward it in foreground runs, or keep a supervisor-owned pipe open for later attachment in background runs (otherwise stdin is `\\.\NUL`) |
| `-t`, `--tty` | Record TTY preference on container metadata (pipe-backed streams; no ConPTY yet) |
| `--pull=missing` | Default: use the cached selection, or pull if missing |
| `--pull=always` | Pull before creating or running; a failed pull fails the command |
| `--pull=never` | Use only local images; a missing image is an error |
| `-e NAME=VALUE`, `--env NAME=VALUE` | Set a variable; the last setting wins |
| `-e NAME`, `--env NAME` | Inherit a host variable, or unset it if absent |
| `-v SOURCE:TARGET[:rw]`, `--volume SOURCE:TARGET[:rw]` | Mount a named volume or host directory at `${data}` or a data subpath |
| `--mount type=volume\|bind,source=SOURCE,target=TARGET` | Explicitly select a writable directory mount |
| `--verbose` | Print runtime diagnostics |

Each `run` or `create` allocates an isolated container under `containers/<id>/`
with its own `state.json` and `data/` directory. Launch creates `container.log`
and prepares `${data}`, `HOME`, `APPDATA`, `LOCALAPPDATA`, `TEMP`, cache, and the
per-container HKCU hive. Multiple containers—
including different versions of the same app—can run concurrently without sharing
state unless mounted storage is explicitly shared. Containers persist after exit
unless `--rm` is set. Docker-style `-p` port publishing remains deferred.

`diff CONTAINER` (also `container diff`) compares files and directories with the
image's initial data, sorted as `A /home/example.txt`, `C /changed.txt`, or
`D /deleted.txt`. Here `/` means `${data}`. Ordinary images have an empty baseline,
so every surviving entry is an addition. Committed images use their saved data
as the baseline; file content and type changes count, timestamps and ACLs do not.
Mounted storage, image files, host filesystem grants, and container metadata/logs
outside `${data}` are excluded. A mount covering `${data}` produces empty output.
Diff supports created, running, paused, and exited containers; a live scan is
best effort and does not pause the application. See
[persistent storage](docs/persistent-storage.md).

`cp SOURCE DESTINATION` (also `container cp`) copies regular files and directories
between the host and a container. Use `CONTAINER:PATH` for one endpoint; `/` means
`${data}`, and relative container paths use the same root. Declared volumes and
bind mounts are included even before the first start or after exit. Running
containers keep running; paused containers stay paused.

```powershell
quasidock cp .\settings.json node-demo:/home/settings.json
quasidock container cp node-demo:/home/settings.json .\settings-copy.json
quasidock cp '.\project\.' node-demo:/home
```

Directories copy recursively; a source ending in `/.` copies its contents into
the destination. Existing files are replaced and directories are merged without
deleting unrelated files. Destination parents must exist. `-` reads or writes a
plain tar stream; tar input requires an existing destination directory and is
fully validated before destination updates. For binary streams in Windows shells,
use a shell that preserves native binary pipelines, or `cmd /c` redirection:

```powershell
cmd /c "quasidock cp node-demo:/home - > home.tar"
cmd /c "quasidock cp - node-demo:/home < home.tar"
```

Copying preserves modification times and inherits destination security settings.
Links, ownership/ACL preservation, compressed tar input, and progress flags are
unsupported. See [copy behavior and limits](docs/persistent-storage.md#copy-files)
for Windows paths, failure handling, and mounted storage.

`commit [OPTIONS] CONTAINER [REPOSITORY[:TAG]]` saves a runnable image with an
independent copy of private data and declared HKCU state. Running containers
pause during capture and resume afterward; manually paused containers stay
paused. Use `--pause=false` for a best-effort live capture. Mounts and their
contents are excluded. Without a target, commit prints an unnamed image digest
that can be run or tagged. A target supplies the new app ID (final repository
component) and version (tag, default `latest`).

```powershell
quasidock commit -a 'Your Name' -m 'Configured environment' worker my-app:saved
quasidock run --rm my-app:saved
quasidock commit -c 'ENTRYPOINT ["cmd.exe"]' -c 'CMD ["/c","echo hello"]' worker demo
```

Repeatable `-c` / `--change` accepts `CMD` and `ENTRYPOINT` JSON arrays, `ENV`,
`WORKDIR`, and `LABEL`. New containers receive writable data copies at creation;
restarting a container preserves its existing data. See
[commit details and limits](docs/persistent-storage.md#commit-private-data).

`start` and `restart` reuse the same container ID, saved command, image digest,
working directory, private data, mounts, and registry state. They append to the
existing log and never pull a newer image. `start` succeeds without another launch
when a container is already running; `restart` also starts stopped containers.
`start` defaults to background; `start -a` or `-i` attaches to exactly one container,
including one already running. Either flag streams stdout/stderr, and `-i` forwards
stdin only if the container was created with `-i`. Attached start subscribes before
launch, captures initial output, and returns the application's exit code without
printing a container name. `restart` launches in the background. A failed start keeps
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

`attach [OPTIONS] CONTAINER` (also `container attach`) joins the primary process's
live streams for a container launched by `run -d` or `start`. It streams stdout
and stderr separately without replaying historical logs. It forwards stdin when
the container has saved `-i` and stdin remains open; `--no-stdin` selects an output
viewer. Multiple viewers may connect, but only one session may own stdin. A second
writer is rejected with guidance to use `--no-stdin`.

Ctrl+C requests TERM through targeted Ctrl+Break and keeps waiting; applications
may ignore it. `--sig-proxy=false` makes Ctrl+C disconnect with exit code 130 and
leaves the container running. Both `attach` and attached `start` support that flag
and `--detach-keys`. Interactive `-t` sessions with console stdin detach with Ctrl+P
then Ctrl+Q and return 0; use, for example, `--detach-keys=ctrl-x,x` to override the
sequence or `--detach-keys=` to disable it. Redirected stdin preserves all bytes.
The client restores console input modes after disconnecting; no ConPTY is allocated.

Keyboard detachment or a lost client releases stdin without closing it. Actual
stdin EOF closes the process's input for that run, allowing pipelines to finish;
later attachments can still view output. Attach returns the primary process's exit
code after draining output, including for `--rm` containers. Stop, kill, and restart
end attachments to the old run; attach never follows a replacement run. A viewer
that exceeds its bounded output queue or stalls pipe writes for five seconds is
disconnected so other viewers and logging can continue. Foreground `run` attachment
is unsupported; its native Ctrl+C behavior is preserved. Older live supervisors must
be stopped and started with this binary before attachment is available.

`exec [OPTIONS] CONTAINER COMMAND [ARG...]` (also `container exec`) runs an
explicit command in a running container's existing sandbox, data, mounts, and
registry session. Options stop at CONTAINER; later flags belong to COMMAND.
Commands use the effective working directory, image deployment, and exec `PATH`
for executable lookup, without substituting the image's entrypoint or default
command. Shell expressions require an explicit shell such as `cmd.exe /c`.

Exec inherits the environment and working directory from container startup,
including saved `run -e` or `create -e` values. Repeatable `-e` / `--env NAME[=VALUE]` overrides
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

Values inherited with `run -e NAME` or `create -e NAME`, including absent variables, are saved when
the container is created and reused across starts. Existing stopped records from
older binaries capture missing snapshots on their first start. Finish active
containers before replacing the binary: live supervisors from older binaries use
a different control protocol.

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

[NGINX](docs/nginx.md) can be built locally from pinned sources with
`quasidock build recipes/nginx.json`. The image serves a default page, supports
TLS and HTTP/2, and keeps configuration, logs, and temporary files in container
data. It is not yet in the published catalog.

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
Node.js, Go, PHP, Composer, Git, Make, CMake/Ninja, and Clang images. Use `--tool TOOL=IMAGE` or
`--stage-tool NAME_OR_INDEX:TOOL=IMAGE` to select native versions. Distro tags do
not select runtime versions; conversion reports substitutions and pins native
dependencies by digest.

### Recipe and image schema (Docker-style)

Quasidock uses Docker-style short image IDs (`node`, `busybox`, `gtk4`, etc.),
required `entrypoint` and optional default `cmd` fields (instead of `command`,
`args`, and `exports`), and `from` base-image aliases (instead of `runtimes`).
See [docs/recipes.md](docs/recipes.md) for the current schema.
