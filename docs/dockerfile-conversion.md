# Dockerfile conversion

`quasidock convert` translates a supported single-stage Linux Dockerfile into a
native Windows recipe. Review the generated JSON, then build and run it normally:

```powershell
quasidock convert . --id my-app
quasidock build .
quasidock run --rm my-app
```

Conversion parses commands and resolves native image metadata. Application
commands execute during `build`. Application code, npm scripts, and dependencies
must themselves work on Windows; conversion success does not verify them.

## Command

```text
quasidock convert [CONTEXT] [-f DOCKERFILE] [-o RECIPE|-]
  [--id ID] [--version VERSION] [--build-arg NAME=VALUE]...
  [--base QUASIDOCK_IMAGE] [--force]
```

The context defaults to the current directory, input to `CONTEXT/Dockerfile`,
output to `CONTEXT/quasidock.json`, ID to the context directory name, and version
to `dev`. Invalid directory names require `--id`. Explicit `-f` and `-o` paths
are relative to the caller's directory. The Dockerfile must be inside the context.

`-o -` writes JSON to stdout with paths relative to the context directory, suitable
for saving as `CONTEXT/quasidock.json`. Diagnostics go to stderr. An existing
recipe requires `--force`; conversion errors leave it unchanged.

## Compatibility

| Input | Conversion |
| --- | --- |
| `FROM node[:VERSION]` | Native `node` base |
| `FROM golang[:VERSION]` | Native `go` base; the toolchain remains available at runtime |
| `FROM python[:VERSION]` | Native `python-sdk` base with pip and normal Python import paths |
| Numeric tags with Alpine, slim, bullseye, bookworm, or trixie suffixes | Match the numeric version against native images; report the substitution |
| `ARG` / `ENV` | Track global/stage scope, defaults, overrides, and instruction-time environment snapshots |
| `COPY` | Live files, directories, multiple sources, ordinary globs, directory contents, and file renaming |
| `WORKDIR` | App-relative build directory and final runtime working directory |
| `RUN npm ci/install/run/prune` | Local project commands; global installs and custom prefixes are unsupported |
| `RUN go mod download` / `go build -o NAME ...` | Native Windows build; translate the output and matching startup path to `.exe` |
| `RUN pip install ...` / `python -m pip install ...` | Install Windows wheels into the app's `.python` directory; expose it through `PYTHONPATH` |
| `RUN node ...` | Run Node on the native toolchain |
| `RUN cd/mkdir/cp/rm` | Literal app-local paths and simple flags |
| Command syntax | JSON exec form or simple POSIX shell commands with assignments and `&&` |
| `ENTRYPOINT` / `CMD` | Native language executables or explicit app executable paths; JSON argument composition |
| Shell-form startup | Native language commands through a pinned BusyBox runtime; shell variables remain runtime variables |
| OCI title, description, URL, license labels | Corresponding recipe metadata |
| Other labels / `EXPOSE` | Advisory diagnostics; ports do not automatically create Quasidock services |

`docker.io/library/...` and short official names are equivalent. Numeric versions
match their specified components: `24` matches `24.x`, while `3.14.10` requires
that patch version. Cached matching images are preferred; otherwise the registry's
highest matching numeric tag is resolved. Omitted tags and `latest` select the
current image. All generated dependencies use fully qualified manifest digest pins.

Missing versions, unknown tags, and Linux image digests require an explicit native
replacement, for example:

```powershell
quasidock convert . --id my-app --base node:24.22.2
```

The replacement must be a standalone native image of the expected family. The
converter reports the chosen image and version. `QUASIDOCK_REGISTRY` and existing
registry credentials apply. No Linux image layers are downloaded.

## Contexts and execution

Each COPY reads the live context at build time. `.dockerignore` applies, including
`!` exceptions and Dockerfile-specific ignore-file precedence. `.gitignore` has no
effect. The recipe being built is excluded from its context. Archives copied by
COPY remain ordinary files.

Context snapshots feed the build cache and are rechecked for source additions,
deletions, content changes, and ignore-file edits. Rebuild after editing sources;
reconvert after changing the Dockerfile or build arguments.

Paths map into the app tree: `/app/server.js` becomes `${app}/app/server.js` at
runtime. Each RUN receives that instruction's environment and working directory.
Global ARG values must be redeclared after FROM before use inside a stage. ARG
values appear in build-stage metadata but are not automatically runtime variables;
do not use ordinary build arguments for secrets.

Generated recipes enable build/runtime networking and use the native base's
sandbox default. Python's `python-sdk` is separate from the existing embeddable
`python` image. Python startup uses the interpreter or `python -m`; generated
pip console-script launchers are not rewritten for deployment.

Images carrying `workdir` use app config v3. Older clients reject that media type.
Existing recipes without a workdir retain v2 image configs and their previous
runtime directory behavior. A configured workdir also applies to command overrides
and is preserved through detached starts and restarts.

## Unsupported input

Additional FROM stages, COPY flags (including `--from`), ADD, apt/apk and other
system package managers, USER, VOLUME, custom SHELL, health checks, stop signals,
ONBUILD, custom Dockerfile frontends, platform overrides, BuildKit mounts/secrets, ownership/mode changes,
here-documents, loops, pipes, redirection, command substitution, and unknown build
commands fail conversion. Shell-form CMD combined with ENTRYPOINT requires a JSON
argument array instead.

Managed path variables such as PATH, HOME, TEMP, PREFIX, and PYTHONPATH require a
native recipe. Explicit Linux Go targets are rejected. Python source builds,
editable installs, custom installation destinations, and Linux-only wheels need
manual packaging. Context links/junctions, escaping paths, Windows-invalid names,
and case collisions are rejected. Literal values colliding with Quasidock's
`${app}`, `${data}`, or other native placeholders need application configuration
files instead; unquoted shell globs and home expansion need a native recipe.

## Verification

The Go tests cover parsing, diagnostics, local registry resolution, live contexts,
cache changes, image config compatibility, and foreground/detached working
directories. `tests/dockerfile.ps1` builds and runs real Node.js, Go, and Python
examples, including npm installation and a local Python wheel import. It uses an
isolated store and can seed source downloads from a pre-existing cache.
