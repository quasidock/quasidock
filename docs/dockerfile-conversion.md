# Dockerfile conversion

`quasidock convert` translates a supported Linux Dockerfile, including multi-stage
builds, into a native Windows recipe. Review the generated JSON, then build and run it:

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
  [--target NAME] [--base QUASIDOCK_IMAGE]
  [--stage-base NAME_OR_INDEX=QUASIDOCK_IMAGE]... [--force]
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
| `FROM golang[:VERSION]` | Native `go` base; present at runtime when used by the selected target |
| `FROM python[:VERSION]` | Native `python-sdk` base with pip and normal Python import paths |
| Multiple `FROM` stages | Isolated native build units; `--target NAME` selects the final image stage, defaulting to the last stage |
| `FROM <earlier-stage>` | Inherit files, scoped variables, labels, workdir, and startup configuration |
| `FROM scratch` | Empty app filesystem with explicit native executable startup |
| Numeric tags with Alpine, slim, bullseye, bookworm, or trixie suffixes | Match the numeric version against native images; report the substitution |
| `ARG` / `ENV` | Track global/stage scope, defaults, overrides, and instruction-time environment snapshots |
| `COPY` | Live files, directories, multiple sources, ordinary globs, directory contents, and file renaming |
| `COPY --from=NAME_OR_INDEX` | Select completed outputs from an earlier stage by name or zero-based index |
| `WORKDIR` | App-relative build directory and selected target's runtime working directory |
| `RUN npm ci/install/run/prune` | Local project commands; global installs and custom prefixes are unsupported |
| `RUN go mod download` / `go build -o NAME ...` | Native Windows build; carry `.exe` substitutions through stage copies and startup paths |
| `RUN pip install ...` / `python -m pip install ...` | Install Windows wheels into the app's `.python` directory; expose it through `PYTHONPATH` |
| `RUN node ...` / `python ...` | Native interpreter execution, including scripts, `-c`/`-e`, and Python modules |
| `RUN cd/mkdir/cp/mv/rm/touch` | App-local filesystem operations; preserve known native executable names through copies, moves, and removal |
| Portable utilities | BusyBox `echo`, `printf`, `pwd`, `cat`, `head`, `tail`, `grep`, `sort`, `uniq`, `wc`, `tr`, `tee`, `true`, and `false` |
| Native app commands | Explicit `.exe`, `.cmd`, or `.bat` paths, or known generated executable names |
| Python console commands | Resolve installed `console_scripts` metadata in the app's `.python` tree |
| RUN syntax | Literal JSON arguments, or POSIX shell words with assignments, globs, `;`, `&&`, `||`, pipelines, and file/descriptor redirection |
| `ENTRYPOINT` / `CMD` | Native language executables, Python console commands, or explicit app executable paths; JSON argument composition |
| Shell-form startup | Native language commands through a pinned BusyBox runtime; shell variables remain runtime variables |
| OCI title, description, URL, license labels | Corresponding recipe metadata |
| Other labels / `EXPOSE` | Advisory diagnostics; ports do not automatically create Quasidock services |

## Native bases and overrides

`docker.io/library/...` and short official names are equivalent. Numeric versions
match their specified components: `24` matches `24.x`, while `3.14.10` requires
that patch version. Cached matching images are preferred; otherwise the registry's
highest matching numeric tag is resolved. Omitted tags and `latest` select the
current image. All generated dependencies use fully qualified manifest digest pins.

If a requested numeric version has no native match, or the Dockerfile uses an
unknown tag or Linux image digest, select an explicit native replacement:

```powershell
quasidock convert . --id my-app --base node:24.22.2
```

The replacement must be a standalone native image of the expected family. The
converter reports the chosen image and version. `QUASIDOCK_REGISTRY` and existing
registry credentials apply. No Linux image layers are downloaded.

`--base` replaces only the selected target's concrete external base. Repeatable
`--stage-base NAME_OR_INDEX=IMAGE` options select replacements for individual
stages, including the target. Names are case-insensitive; indexes start at zero.
Both `--target preview` and `--target=preview` forms are accepted, as are
`--stage-base build=go:1.26.0` and `--stage-base=build=go:1.26.0`.

Overrides must refer to concrete external bases and preserve their native family.
An inherited target receives its ancestor's replacement; supply `--stage-base`
for that ancestor. Scratch stays empty. Unknown selectors, duplicate overrides
for one stage (including name/index aliases), and combining `--base` with a
`--stage-base` override for the selected target are errors. Selectors and base
eligibility are checked even for unused stages; their native image references
are resolved only if the stages are needed.

## Multi-stage conversion

`--target NAME` selects a named stage, with the last stage as the default. Target
names are case-insensitive; numeric indexes are accepted by `--stage-base` and
`COPY --from`, while `--target` requires a name. The selected target supplies
the runtime configuration and must have a supported startup command. Selecting
an artifact-only Go or scratch builder without startup is an error.

Only the selected target and its dependencies are translated and resolved. Unused
stages are reported and skipped, including branches with Linux-only bases or
commands. Syntax and invalid stage references still fail the whole file.

Each unrelated FROM starts with fresh files, variables, and native tools. A
`FROM <earlier-stage>` stage inherits its parent's files, ARG/ENV values, labels,
workdir, startup configuration, and executable mappings. Parent commands run
once; child writes and deletions affect the child's filesystem. A COPY from
another stage transfers files without inheriting its runtime configuration.

Only the selected target supplies the application's runtime configuration and
dependencies. Builder ENTRYPOINT/CMD values are compiled for startup only when
inherited by that target. Different stages can use different versions of the
same native tool without adding builder tools to the runtime image.

The generated recipe represents the target at the top level and its dependencies
as isolated [build units](recipes.md#build-units). Each unit runs once in
dependency order, and stage copies read its completed output.

For example:

```dockerfile
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN go build -o /out/server .

FROM scratch AS runtime
COPY --from=build /out/server /app/server
CMD ["/app/server"]
```

Convert the runtime target and choose the builder's native Go image:

```powershell
quasidock convert . --id my-app --target runtime --stage-base build=go:1.26.0
quasidock build .
quasidock run --rm my-app
```

`--stage-base 0=go:1.26.0` selects the same builder. The runtime target uses
scratch, so its Go replacement belongs on `build` rather than in `--base`.

The recipe builds `server.exe`, copies it to `app/server.exe`, and launches that
path. The Go toolchain stays in the build unit. Executable selectors and globs
use the Dockerfile's logical names; `.exe` is preserved when copying or renaming
their native outputs. Literal filenames that conflict with these substitutions
are rejected. Generated recipes record this mapping in `executables` metadata.

Stage-copy source paths start at the producing stage's app root, independent of
its WORKDIR. Destinations use the receiving stage's WORKDIR. Directory copies
select contents, and multiple sources require a destination ending in `/`.
Snapshots contain mapped app output, rather than a Linux base filesystem;
interpreter/system-library paths have no automatic native translation.

To select an earlier runnable target with inherited configuration:

```dockerfile
FROM node:24 AS base
WORKDIR /app
COPY . .
CMD ["node", "server.js"]

FROM base AS preview
ENV MODE=preview

FROM base AS production
ENV MODE=production
```

```powershell
quasidock convert . --id my-app --target preview --stage-base base=node:24.22.2
```

The image uses preview's environment and the inherited startup, workdir, files,
and native Node base. The production branch is skipped. `--base` cannot replace
preview's inherited base; select its concrete ancestor with `--stage-base base=...`.

## Contexts and execution

Each context COPY reads live files at build time. `.dockerignore` applies, including
`!` exceptions and Dockerfile-specific ignore-file precedence. `.gitignore` has no
effect. The recipe being built is excluded from its context. Archives copied by
COPY remain ordinary files.

`COPY --from` reads independent snapshots captured after the producing stage
finishes. Ignore files do not filter those outputs. Child writes and deletions
affect their own copies.

Context snapshots feed the build cache and are rechecked for source additions,
deletions, content changes, and ignore-file edits. Cache inputs also include
reachable units, dependency pins, commands, and instruction environments. The
cache currently reuses the complete build; unit outputs are not independently
cached. `--rebuild`, `--clean`, and `--keep-build-dir` apply across the build graph.
Source publication and dependency provenance include all executed units.

Rebuild after editing sources. Reconvert after changing the Dockerfile, build
arguments, target, or native base selections.

Paths map into the app tree: `/app/server.js` becomes `${app}/app/server.js` at
runtime. Each RUN receives that instruction's environment and working directory.
`cd` changes only the current RUN. Use `cd /directory && command` when subsequent
commands require that directory. Conversion rejects relative paths after branches
that can leave different working directories, and executable or directory mapping
changes inside `||` and pipelines. Separate those operations into RUN instructions.

The converter translates known file operands and file-bearing options, preserving
patterns, URLs, package specifications, and inline code. Arguments to custom app
executables and Python console commands are opaque; use relative paths for their
application files. Utilities with filesystem operands accept a bounded set of
BusyBox short options; conversion rejects unknown options instead of guessing
their path semantics.
Redirects support `<`, `>`, `>>`, `<&`, and `>&`; `/dev/null` maps to Windows' null
device. Pipelines return the last command's status, and sequences return the last
statement's status. Conversion does not implicitly enable `errexit` or `pipefail`.

Shell globs expand during build; an unmatched pattern remains literal, allowing
`RUN pip install flask==3.0.*`. Quotes suppress globbing. JSON RUN arguments never
undergo shell expansion: `RUN ["echo", "$VALUE", "a*b"]` prints those literal
arguments. Globs that cannot preserve a known executable's logical/native name
are rejected; use its explicit logical path instead.

Global ARG values must be redeclared after FROM before use inside a stage.
Unrelated stages do not share stage-local ARG/ENV values; child stages inherit
those declared in their parent. ARG values appear in build-stage metadata but
do not automatically become runtime variables. Do not use ordinary build arguments
for secrets.

Generated recipes enable build/runtime networking and use the native base's
sandbox default; scratch uses the normal native default. Python's `python-sdk`
is separate from the existing embeddable
`python` image. Python console commands run through that image's interpreter using
installed entry-point metadata, so temporary paths in pip-generated launchers are
not used. RUN resolves console commands when executed; the selected target's
startup console commands receive a final build-time metadata check. Missing or
duplicate entry points fail the build. Arguments and exit codes are preserved.

For example, supply `hello.py` defining a Flask application and convert:

```dockerfile
FROM python:3.14
RUN pip install flask==3.0.*
COPY hello.py /
ENV FLASK_APP=hello
CMD ["flask", "run", "--host", "0.0.0.0", "--port", "8501"]
```

Images carrying `workdir` use app config v3. Older clients reject that media type.
Existing recipes without a workdir retain v2 image configs and their previous
runtime directory behavior. A configured workdir also applies to command overrides
and is preserved through detached starts and restarts.

## Diagnostics

Conversion diagnostics identify the Dockerfile line, instruction, and stage
name/index for multi-stage input. Notes report target selection, skipped stages,
native image mappings, and executable substitutions. Context COPY selectors and
literal executable collisions are checked before the recipe is written and
checked again against live inputs during builds.

Missing generated files and invalid generated output paths fail during `build`.
Generated recipes do not yet retain Dockerfile source locations for build-time
errors; use the reported build unit and recipe step to locate the failing copy.

## Unsupported input

Duplicate stage names, unknown or out-of-range stage references, self-references,
forward stage references, external-image/named-context stage copies, COPY flags
other than `--from`, ADD, apt/apk and other
system package managers, USER, VOLUME, custom SHELL, health checks, stop signals,
ONBUILD, custom Dockerfile frontends, platform overrides, BuildKit mounts/secrets, ownership/mode changes,
here-documents, loops, functions, background commands, negation, command substitution,
arithmetic, and unknown build
commands fail conversion. Shell-form CMD combined with ENTRYPOINT requires a JSON
argument array instead.

Managed path variables such as PATH, HOME, TEMP, PREFIX, and PYTHONPATH require a
native recipe. Explicit Linux Go targets are rejected. Python source builds,
editable installs, custom installation destinations, and Linux-only wheels need
manual packaging. Context links/junctions, escaping paths, Windows-invalid names,
and case collisions are rejected. Literal values colliding with Quasidock's
`${app}`, `${data}`, or other native placeholders need application configuration
files instead; home expansion needs a native recipe. Shell-form startup retains
the simple-command/`&&` subset; the broader shell operators apply to RUN.
Copies from Linux Python virtual environments or site-packages paths need manual
packaging; dependency copies from the native `.python` tree are supported.

## Verification

The Go tests cover parsing, diagnostics, local registry resolution, live contexts,
cache changes, executable mappings, stage isolation, image config compatibility,
and foreground/detached working directories. A CLI integration test converts,
builds, and runs a three-stage native executable example, rebuilds live edits,
and selects and runs an earlier target. Target/override tests cover inherited
bases, conflicts, unused branches, native family checks, and digest pinning.
`tests/dockerfile.ps1` currently builds and runs single-stage Node.js, Go, and Python
examples, including native shell pipelines, executable renames, a local Python
wheel console command, argument/exit-code checks, missing/duplicate metadata,
and a Flask wildcard install with an HTTP response check. It uses an
isolated store and can seed source downloads from a pre-existing cache.
