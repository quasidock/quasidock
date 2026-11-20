# Dockerfile conversion

`quasidock convert` translates a supported Linux Dockerfile, including multi-stage
builds, into a native Windows recipe. Review the generated JSON, then build and run it:

```powershell
quasidock convert . --id my-app
quasidock build .
quasidock run --rm my-app
```

Conversion parses commands, resolves native image metadata, and checks the paths
selected by context COPY instructions without reading or hashing their contents.
Application commands execute during `build`. Application code, npm scripts, and
dependencies must themselves work on Windows; conversion success does not verify them.

## Command

```text
quasidock convert [CONTEXT] [-f DOCKERFILE] [-o RECIPE|-]
  [--id ID] [--version VERSION] [--build-arg NAME=VALUE]...
  [--target NAME] [--base QUASIDOCK_IMAGE]
  [--stage-base NAME_OR_INDEX=QUASIDOCK_IMAGE]...
  [--tool TOOL=QUASIDOCK_IMAGE]...
  [--stage-tool NAME_OR_INDEX:TOOL=QUASIDOCK_IMAGE]... [--force]
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
| `FROM php[:VERSION[-cli][-DISTRO]]` | Native `php` CLI base; Apache, FPM, and ZTS variants are unsupported |
| `FROM composer[:VERSION]` | Composer's PHAR with a separately selected native PHP interpreter |
| `FROM ubuntu[:TAG]` / `debian[:TAG]` | Distro metadata with native tools selected by supported apt installs |
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
| `RUN php ...` / `composer ...` | Native PHP scripts, lint, inline code, built-in server, and local Composer project commands |
| `RUN apt/apt-get update/install/clean/autoclean` | Translate the package catalog below into pinned native dependencies |
| `RUN git/make/cmake/ninja` | Native tools with common directory/file options mapped into the app tree |
| `RUN gcc/g++/clang/clang++/cc/c++` | Native Clang; object compilation and explicit `-o` executable linking with `.exe` mappings |
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
PHP additionally accepts `cli` and versioned `-cli` tags with Debian or Alpine
suffixes. Native PHP packaging revisions such as `8.5.12-1` match Docker's
`8.5.12` version; the highest matching revision is preferred.

If a requested numeric version has no native match, or the Dockerfile uses an
unknown tag or Linux image digest, select an explicit native replacement:

```powershell
quasidock convert . --id my-app --base node:24.22.2
```

The replacement must be a standalone native image of the expected family, except
Composer, whose own layer is imported as application files. The
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

## Ubuntu/Debian and apt packages

Official Ubuntu and Debian names, including `docker.io/library/...`, support
numeric tags, release names, and slim tags. The tag describes the source distro;
it does not select native runtime versions or provide a Linux filesystem. Distro
image digests are rejected; use a distro tag and native tool digest pins instead.
Conversion reports substitutions and resolves each tool's current native image
independently. Generated references are pinned to manifest digests.

| Apt package | Native image | Tool alias |
| --- | --- | --- |
| `python3`, `python3-pip`, `python3-dev` | `python-sdk` | `python` |
| `nodejs`, `npm` | `node` | `node` |
| `golang`, `golang-go` | `go` | `go` |
| `php`, `php-cli` | `php` | `php` |
| `composer` | `composer` (own layer) and `php` | `composer`, `php` |
| `git` | `mingit` | `git` |
| `make` | `make` | `make` |
| `cmake`, `ninja-build` | `cmake` (includes Ninja) | `cmake` |
| `clang`, `gcc`, `g++` | `llvm-mingw` | `compiler` |
| `build-essential` | `llvm-mingw` and `make` | `compiler`, `make` |

For example, this converts to the native Python/Flask setup:

```dockerfile
FROM ubuntu:24.04
EXPOSE 8501
RUN apt-get update && apt-get install -y python3 python3-pip
RUN pip install flask==3.0.*
COPY hello.py /
ENV FLASK_APP=hello
CMD ["flask", "run", "--host", "0.0.0.0", "--port", "8501"]
```

Both apt commands accept `-y`, `--yes`, `--assume-yes`, `-q`, `-qq`, and
`--no-install-recommends`. Package names may use declared ARG/ENV values resolving
to individual names. JSON RUN forms are supported. Update, clean, autoclean, and
the exact shell command `rm -rf /var/lib/apt/lists/*` become successful no-ops
with diagnostics. No apt database is created.

Apt also works in Debian-derived language bases, including default and slim
tags. Alpine variants and scratch do not support apt. Installs may appear in
separate statements or apt-only `&&` setup chains. Installs gated by other
commands, installs in `||` or pipelines, and apt redirections are rejected;
move the install into its own RUN.

Tools become available to subsequent translated commands, and repeated installs
coalesce. Each stage retains its installed tools; children inherit them and may
add more. Copies transfer files without inheriting tools. Builder-only tools
stay in their build units. Distro targets require an explicit supported CMD or
ENTRYPOINT. Their sandbox defaults to `soft` when a resolved dependency selects
it, otherwise to the normal native default.

Select native versions by tool alias:

```powershell
quasidock convert . --tool python=python-sdk:3.14.10
quasidock convert . --stage-tool build:compiler=llvm-mingw:20261020
```

Repeat `--tool TOOL=IMAGE` for global selections or
`--stage-tool NAME_OR_INDEX:TOOL=IMAGE` for one stage. Names are case-insensitive;
indexes start at zero. A stage selection overrides the global selection and must
address the stage that first supplies the tool, including a language base.
Inherited tools keep their ancestor's selection. Unknown aliases/selectors,
duplicate name/index aliases, and conflicting base/tool selections fail
conversion. Known unused selections are reported without resolving them. Native
images must match the catalog's image ID and have no base images of their own,
except Composer's source image.
Distro bases use tool options rather than `--base` or `--stage-base`; existing
language-base options remain available.

Git maps app paths in `-C`, `--git-dir`, and `--work-tree`; Make in `-C`, `-f`,
`-I`, `--directory`, `--file`, and `--include-dir`; CMake in `-S`, `-B`, `--build`,
`--install`, and `--prefix`; Ninja in `-C` and `-f`. Other arguments remain opaque
and need relative app paths. Unmapped absolute Linux path arguments are rejected;
URLs remain ordinary arguments.

Direct compilers map source/object paths, `-I`, `-L`, `-isystem`, `-iquote`,
`-include`, and `-o` (including attached forms). Executable links require an
explicit `-o`; `.exe` substitutions survive copies, moves, and startup. `-c`,
`-S`, and `-E` outputs retain their filenames. Shared/relocatable linking,
target/sysroot overrides, compiler escape options, and options with unknown
operand semantics require a native recipe. Common optimization, debug, warning,
language-standard, macro, and library flags are supported.
GCC packages use Clang targeting Windows, with a substitution diagnostic.
`build-essential` supplies the compiler and Make rather than Debian packaging
tools or Linux headers. Scripts, Makefiles, and CMake projects must themselves
support Windows tools. Conversion does not rewrite them or infer executable
names generated by build systems; use explicit `.exe` paths for those outputs.

Unknown packages, curl, package version constraints, `.deb` files, repository
changes, upgrades, and removals are unsupported. Use native image overrides for
tool versions. Mappings may supply bundled extras, such as pip with Python or
Ninja with CMake.

## PHP and Composer

PHP commands map script paths and the file options `-c`, `-f`, `-F`, and `-t`
(including attached and long forms) into the app tree. Inline PHP code, INI
definitions, server addresses, and script arguments remain literal data. For
example, with `public/index.php` in the build context:

```dockerfile
FROM php:8.5-cli-bookworm
WORKDIR /app
COPY . .
RUN php -l /app/public/index.php
CMD ["php", "-S", "0.0.0.0:8080", "-t", "/app/public"]
```

PHP uses the native image's bundled configuration and extensions. Linux PHP
configuration locations, `.so` extensions, `docker-php-ext-*`, `phpize`, PECL
install commands, `install-php-extensions`, Apache helpers, and FPM need manual
native packaging. Conversion does not replace Apache with PHP's built-in server.

Composer imports its pinned image's own layer under the app's `.composer`
directory and runs `composer.phar` with the stage's native PHP. Its image's
bundled PHP is not added as a runtime. `FROM composer` supplies both tools;
Debian-derived PHP stages can add Composer with `RUN apt install composer`.
Select versions independently with `--tool php=php:VERSION` and
`--tool composer=composer:VERSION`, or their stage-specific equivalents.
Composer's `-d` / `--working-dir` paths are mapped; global commands and
self-update are unsupported.

Composer builders can pass installed dependencies to a PHP target:

```dockerfile
FROM composer:2 AS vendor
WORKDIR /app
COPY . .
RUN composer install --no-dev --no-interaction --prefer-dist

FROM php:8.5-cli
WORKDIR /app
COPY --from=vendor /app /app
CMD ["php", "/app/main.php"]
```

Composer scripts and dependencies must support Windows. Builder tools and the
root-level `.composer` directory stay in the builder when only `/app` is copied.
Copies from external images, such as `COPY --from=composer:2 /usr/bin/composer`,
remain unsupported; use a named Composer build stage or the apt mapping.

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
other than `--from`, ADD, apk and system package managers outside the supported
apt subset, USER, VOLUME, custom SHELL, health checks, stop signals,
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
`tests/dockerfile.ps1` builds and runs single-stage Node.js, Go, Python, and PHP
examples, including native shell pipelines, executable renames, a local Python
wheel console command, argument/exit-code checks, missing/duplicate metadata,
an Ubuntu apt/Flask wildcard install with an HTTP response check, PHP lint and
literal arguments/exit codes, its built-in HTTP server, and an offline Composer install passed through an
inherited build stage to a PHP runtime. Use `-Language php` for the PHP/Composer
cases. Add
`-NativeTools` to pull existing Git, compiler, Make, and CMake images and test a
Debian mixed-tool build plus a multi-stage C executable copied into scratch.
It uses an
isolated store and can seed source downloads from a pre-existing cache.
