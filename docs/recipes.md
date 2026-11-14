# Quasidock Recipes

Quasidock builds `windows/amd64` OCI images from UTF-8 JSON recipe files
(`quasidock build [recipe.json|directory]`). When invoked with no argument or
with a directory, `quasidock build` looks for `quasidock.json` in that
directory.

For container run examples and CLI usage, see [README.md](../README.md) or run
`quasidock help`.

## Recipe structure

```json
{
  "id": "node",
  "version": "24.14.1",
  "name": "Node.js",
  "summary": "JavaScript runtime built on V8",
  "homepage": "https://nodejs.org/",
  "license": "MIT",
  "entrypoint": "node.exe",
  "permissions": {
    "sandbox": "low",
    "filesystem": ["cwd"]
  },
  "stages": [
    {
      "name": "node",
      "sources": [
        {
          "url": "https://nodejs.org/dist/v24.14.1/node-v24.14.1-win-x64.zip",
          "sha256": "...",
          "strip": 1
        }
      ]
    }
  ]
}
```

### Top-level fields

| Field | Required | Description |
| --- | --- | --- |
| `id` | Yes | Short Docker-style image ID, `1`–`57` characters matching `[A-Za-z0-9._-]`, not starting with `.`, and not `catalog` (for example `node`, `busybox`, `gtk4`). |
| `version` | Yes | Image version string. |
| `entrypoint` | Yes | Non-empty JSON string or string array defining the default executable and any fixed leading arguments (supports placeholders). |
| `cmd` | No | String array of default arguments appended after `entrypoint` when running without a command. |
| `stages` | Yes | Non-empty array of build stages (`Stage` objects). |
| `name` | No | Display name shown in metadata (`1`–`64` bytes on one line). |
| `summary` | No | Short one-line summary (`1`–`160` bytes on one line). |
| `homepage` | No | `https://` URL (at most 256 bytes, without spaces). |
| `license` | No | SPDX license identifier or expression (for example `"GPL-2.0-or-later"` or `"MIT OR Apache-2.0"`). |
| `path` | No | String array of directories added to `PATH` when this image runs or provides `from`/`sdk` tools. Defaults to `["."]`. |
| `env` | No | Map of environment variables set for the image and downstream builds; values support placeholders. |
| `permissions` | No | Sandbox, network, filesystem, host, and service permissions object. |
| `registry` | No | Array of `HKCU\Software\...` subkeys persisted per container (for example `"Software\\JavaSoft\\Prefs\\net\\sourceforge\\plantuml"`). Excludes `Software` itself, `Classes`, `Microsoft`, `Policies`, `Wow6432Node`, `AppDataLow`, and `quasidock`, and keys must not overlap. |
| `from` | No | Map `{ "<alias>": "<image-ref>@sha256:..." }` of pinned base images shipped as lower OCI layers with the app. Single-level only: `from` images cannot declare `from` images of their own. |
| `sdk` | No | Map `{ "<alias>": "<image-ref>@sha256:..." }` of pinned build-time tool images mounted into the build environment (not shipped in the built image). |
| `host` | No | Host toolchains required during build. Currently only `["msvc"]` is accepted, and requires at least one stage with `build` commands. |
| `source_mirrors` | No | Ordered array of fully qualified, digest-pinned images whose source artifacts can supply unavailable URL inputs. Each downloaded file must match the source's `sha256`. |
| `layer` | No | App layer format: `"gzip"` (default) or `"estargz"`. |
| `prefetch` | No | `/`-separated relative paths (case-insensitively unique) prefetched on lazy pull. Requires `"layer": "estargz"`. |
| `cleanup` | No | Files or paths removed after stages complete before packaging the app layer. |

Alias names in `from` and `sdk` must match `[a-z][a-z0-9_-]{0,31}`, cannot be
`app`, `data`, `cache`, or `roots`, and cannot overlap between `from` and `sdk`.

Stages go into the app's files in order, so a later stage can overlay an
earlier one. Every URL that the store's download cache does not already have is
downloaded before anything else, eight at a time; if one download fails,
remaining queued downloads are cancelled. Each source is a single `file` or an
archive (`zip`, `tar`, `tar.gz`/`tgz`, or `tar.xz`/`txz`; `.crate` also maps
to `tar.gz`) extracted into `dest` with optional `strip`. The type is inferred
from the file name or given explicitly with `type`. Inside a tar archive, a link
to another file in the same archive becomes a copy of that file, whereas a link
to a directory or outside the archive fails the build. Other archives such as
`.7z` files or installers come in as `file` sources and are unpacked in `build`
with 7-Zip in `sdk` (`7z x -o"$PREFIX" file.7z`).

When a URL fails or returns different bytes, the builder first checks the source
artifact next to the image's own version, then each `source_mirrors` image.
For example, `"source_mirrors": ["ghcr.io/example/app@sha256:..."]` can recover
inputs published under a previous registry namespace. Mirrors supply only the
requested hash-verified files; their executable layers are never installed.

`cleanup` strips unwanted files after all stages finish: `"/include"` removes a
path from the top of the built tree (case-insensitive), while `"*.pdb"` removes
matching file names anywhere in the tree. Building the same recipe without
changing inputs produces the same image digest.

`name`, `summary`, `homepage`, and `license` describe the image for
`quasidock search` and `quasidock image info`. All four are optional and omitted
from the OCI image config when absent, so adding or omitting metadata only
changes the config when present. `registry` lists `HKEY_CURRENT_USER` keys kept
per container across runs. See [internal/recipe/recipe.go](../internal/recipe/recipe.go)
for the schema implementation.

### Layer formats and `prefetch`

`layer` sets the app layer format: `"gzip"` (the default) or `"estargz"`
([layer formats](layer-formats.md)), a gzip-compressed tar whose files can be
read individually through a table of contents at the end. Quasidock unpacks
eStargz layers without inflating a full intermediate tar on disk—in Zigsaw
measurements, saving 323 MB of temporary disk space when pulling Qt's SDK at
2–5% larger download size. The layer descriptor carries the table-of-contents
digest expected by eStargz tools, and the layer includes `stargz.index.json`
and `.no.prefetch.landmark`. Recipes such as
[qt6.json](../recipes/qt6.json), [qt6-sdk.json](../recipes/qt6-sdk.json),
[llvm-mingw.json](../recipes/llvm-mingw.json), [texlive.json](../recipes/texlive.json),
and [vscodium.json](../recipes/vscodium.json) use `"estargz"`.

`prefetch` lists `/`-separated paths inside the app placed first in an eStargz
layer (in list order, after parent directories) so `quasidock pull --lazy`
fetches them upfront before first run. A path the built tree does not contain
is skipped with a warning so upstream version bumps continue building. Use
[scripts/record-prefetch.sh](../scripts/record-prefetch.sh) to capture a
`prefetch` list from a lazy run:

```bash
scripts/record-prefetch.sh <ref> [--entrypoint=<name>] [-- <args>...]
scripts/record-prefetch.sh <ref> --window=<title> [--entrypoint=<name>]
```

When pulling an eStargz layer (`quasidock pull`), Quasidock inspects indexed
eStargz layers already in the local store, copies matching members locally, and
downloads only the missing byte ranges when at least 10% of the blob can be
reused. The assembled blob is verified against its expected sha256 digest and
falls back to a whole-blob download on mismatch. On Zigsaw, this delta pull
fetched 0.4% of Qt's SDK when 3 files changed and 83% for `llvm-mingw` between
`20261006` and `20261020`. `quasidock image files` and `quasidock image cat`
read a remote eStargz layer's directory and files without pulling the image.

### Placeholders

Placeholders expand at build time (for `from`/`sdk` tool paths and `env`) and at
container runtime (for `entrypoint`, `cmd`, `path`, and `env`):

- `${app}`: the image's own deployment directory.
- `${data}`: the container's isolated data directory (`containers/<id>/data`),
  created fresh on each `quasidock run` (and deleted on exit when `--rm` is
  passed).
- `${cache}`: `${data}\cache` at container runtime; `B:\cache\<tool id>` for
  tool images during `quasidock build`.
- `${roots}`: path to a PEM bundle of trusted root CAs from the Windows store,
  written when referenced (for example `SSL_CERT_FILE` in
  [perl.json](../recipes/perl.json); see [networking](networking.md)).
- `${<alias>}`: the deployment directory of the `from` (or `sdk` at build time)
  image named `<alias>`.
- `${listen:<service>}`, `${port:<service>}`: internal listen port and external
  host port for a declared service.
- `${uses:<name>}`: endpoint port for a declared port dependency in
  `permissions.uses`.

At runtime, `run -v 'NAME:${data}'` attaches a named volume to the complete data
tree. A target such as `${data}/workspace` preserves only that subtree; bind
sources use the same targets. Mounts are run options rather than recipe fields.
Mounted contents survive `--rm`, including registry state when its backing
directory is mounted. See [persistent storage](../README.md#persistent-storage)
for source syntax, sharing, and explicit volume cleanup.

## Entrypoints, `cmd`, `path`, and `env`

Every recipe must define `entrypoint` as a string or non-empty string array.
`entrypoint[0]` must be a relative path inside the app or begin with
`${app}/...`, `${app}\...`, or a `from` base image's `${<alias>}/...` or
`${<alias>}\...`. `quasidock build` checks after all stages finish that
`entrypoint[0]` resolves to an existing file in the app or in the referenced
base image. `cmd` gives default trailing arguments appended after `entrypoint`
when running without a command override.

`path` (default `["."]`) lists directories inside `${app}`, `${data}`,
`${cache}`, or `${<alias>}` added to `PATH`. `env` defines container environment
variables expanded with placeholders (base images' `env` expand before the
app's). In [node.json](../recipes/node.json), Node keeps global npm installs and
their executables inside the container's data directory:

```json
"entrypoint": "node.exe",
"path": [".", "${data}\\npm"],
"env": {
  "NPM_CONFIG_PREFIX": "${data}\\npm",
  "NODE_OPTIONS": "--preserve-symlinks --preserve-symlinks-main",
  "NODE_USE_SYSTEM_CA": "1"
}
```

At runtime (`quasidock run [OPTIONS] IMAGE [COMMAND] [ARG...]`):

- With neither `COMMAND` nor `--entrypoint`, Quasidock runs `entrypoint`
  followed by `cmd`.
- Passing `COMMAND [ARG...]` after `IMAGE` resolves `COMMAND` (relative to
  `${app}`, or across `${app}`, `path`, `from` `path`, and
  `%SystemRoot%\System32` checking `.exe`, `.com`, `.cmd`, `.bat`) and replaces
  both `entrypoint` and `cmd`—unless `COMMAND` matches the image `id` (or the
  stem of `entrypoint[0]` when `entrypoint` has multiple elements), in which
  case `entrypoint` stays intact and `[ARG...]` replaces `cmd`.
- Passing `--entrypoint=COMMAND` overrides `entrypoint` and clears default `cmd`
  (setting `--entrypoint` to the image `id` keeps the configured `entrypoint`
  and its leading arguments while clearing `cmd`; `--entrypoint=""` requires a
  `COMMAND` after `IMAGE`).

See [containers and running images](../README.md#containers-and-running-images)
for run flags and examples.

Zigsaw's `exports`, `aliases`, Windows shortcuts/shims, `override`, `update`,
`--share=commands`, and user-folder junctions were removed in Quasidock
0.2.0–0.3.0; `entrypoint`, `cmd`, and `--rm` replace `command`, `args`,
`exports`, and `--ephemeral` (see
[Legacy Zigsaw differences](#legacy-zigsaw-differences)).

## `permissions`

- `sandbox`: `"soft"`, `"low"` (default), or `"appcontainer"`.
- `network`: boolean enabling network access in sandboxed runs.
- `filesystem`: array of `"cwd"` or absolute Windows paths, optionally suffixed
  with `:ro` or `:rw`.
- `services`: array of `{ "name": "...", "port": <n>, "command": ... }` service
  declarations (`command` is optional).
- `uses`: array of `{ "name": "...", "port": <n> }` service dependencies.
- `unsupported`: map keyed by `"soft"`, `"low"`, or `"appcontainer"` giving a
  one-line reason (at most 256 bytes) why that sandbox mode cannot be used;
  cannot match the image's chosen `sandbox`.
- `host`: optional boolean allowing `quasidock-host`.

Containers run at `low` integrity unless the recipe or `--sandbox` selects
another mode ([sandboxes](../README.md#windows-sandbox-and-quasidock-extensions)).
Apps that fail under low integrity request `"soft"`, such as
[kanboard.json](../recipes/kanboard.json) (where PHP reports files as not
writable at low integrity) and toolchains like `zig`, `rust`, `node`, and SDKs
(because Windows starts a low-labelled executable at low integrity and labels
files it writes low). Omitting `sandbox` leaves it out of the image config and
defaults to `"low"`.

`quasidock pull` runs non-interactively and prints signature status alongside
the image's permissions; `quasidock image info` displays remote permissions. At
runtime, `--sandbox=...`, `--filesystem=...`, `--nofilesystem=...`,
`--share=network|host`, and `--unshare=network|host` override recipe defaults.

An image can explain which sandboxes do not work with `unsupported`:

```json
"permissions": {
  "sandbox": "soft",
  "unsupported": {
    "low": "PHP's is_writable() is false for everything at low integrity, so Kanboard says its data can't be written"
  }
}
```

Running under an `unsupported` sandbox prints the warning before starting and
continues, since an application flag may still work around the limitation.

### Host programs (`quasidock-host`)

```json
"permissions": { "sandbox": "soft", "host": true }
```

When `permissions.host` is `true` (or `--share=host` is passed without
`--unshare=host`), Quasidock copies `quasidock-host.exe` into `<store>\host`
and appends that directory at the end of `PATH` (after `System32`) under `soft`
and `low` sandboxes. Running `quasidock-host <program> [arguments]` runs a
program from your real user `PATH` in your user environment. Under
`appcontainer`, `quasidock-host` is omitted with a note because AppContainer
processes cannot read host-installed files. Recipes should omit `host` unless
actually needed so runs stay independent of the host machine.

## Services

An app that accepts connections from other programs on the host declares
`permissions.services`, as in [postgresql.json](../recipes/postgresql.json) or
[kanboard.json](../recipes/kanboard.json):

```json
"env": { "PGPORT": "${listen:postgresql}" },
"permissions": {
  "network": true,
  "services": [
    { "name": "postgresql", "port": 5432, "command": "bin/pg-server.exe" }
  ]
}
```

Service names must be `1`–`32` lowercase letters, digits, or `-`, with unique
names and distinct ports (`1`–`65535`). Each service provides two placeholders in
`entrypoint`, `cmd`, and `env`:

- `${listen:<name>}`: the port the app binds to (and where sibling processes in
  the same container connect).
- `${port:<name>}`: the external localhost port other programs on this machine
  use (for example Kanboard's `"KANBOARD_URL": "http://127.0.0.1:${port:http}/"`).

Under `soft` and `low`, both placeholders equal the declared port (or the port
passed to `--port=<name>=<port>`). Under `--sandbox=appcontainer`, processes
outside the container cannot connect directly into the container's loopback
([networking](networking.md)):

- `${listen:<name>}` is assigned an internal port (`20000`–`48999`) chosen on
  first run and saved in `<store>\ports\<id>.txt` so runs agree.
- Publishing runs when the command matches `command` (or the app's default
  entrypoint if `command` is omitted; `command` obeys the same path rules as
  `entrypoint[0]`).
- Quasidock listens on the external port on `127.0.0.1` and `::1` outside the
  container and passes accepted sockets to `quasidock-relay.exe`, started inside
  the container's job without network capability, which forwards traffic to the
  internal port.
- If the host port is already taken, the run warns and continues unpublished.

Declaring `services` does not require `"network": true`, which controls outbound
network access. `quasidock image info` and `quasidock pull` list declared
services.

## Ports an app uses

An app that connects to a local server declares its target ports under
`permissions.uses`, as in [osm2pgsql.json](../recipes/osm2pgsql.json):

```json
"env": { "PGPORT": "${uses:postgresql}" },
"permissions": {
  "network": true,
  "uses": [
    { "name": "postgresql", "port": 5432 }
  ]
}
```

Port names follow service naming rules (`1`–`32` lowercase letters, digits, or
`-`) and cannot duplicate another entry in `uses` or collide with `services`.
`${uses:<name>}` expands in `entrypoint`, `cmd`, and `env`:

- Under `soft` and `low`, it is the declared port (or the port passed to
  `--port=<name>=<port>`).
- Under `--sandbox=appcontainer` with `"network": true`, Quasidock grants a
  temporary loopback exemption while the app runs (unless
  `QUASIDOCK_LOOPBACK_EXEMPTION=0` is set or Windows refuses), so `${uses:<name>}`
  also resolves directly to the host port.
- When an AppContainer run is not exempt (no network, OS refused, or
  `QUASIDOCK_LOOPBACK_EXEMPTION=0`), Quasidock allocates a local relay port
  inside the container and connects each incoming connection through
  `quasidock-relay.exe` to the host port ([networking](networking.md)).

Declared `uses` ports are reachable in every sandbox, with or without outbound
network access; an app explicitly targeting a different undeclared port
bypasses forwarding. Base images in `from` may also declare `uses`, which merge
with the app's own `uses` (with the app's definition winning on duplicate
names).

## Base images (`from`)

An app can build and run on top of other Quasidock images using `from`, rather
than bundling every dependency into one layer. For example,
[prettier.json](../recipes/prettier.json) pins Node as `"node"`:

```json
"entrypoint": ["${node}\\node.exe", "${app}\\bin\\prettier.cjs"],
"from": {
  "node": "node:24.22.2@sha256:cab43fbbd7944f841db320f6cb3530df85afe92709fe93c240db4f0ca75ff4b4"
}
```

And [meld.json](../recipes/meld.json) combines multiple base images—including
tools like `git` (`mingit`) that are added to `PATH` and `env` without copying
their files into Meld's own layer:

```json
"from": {
  "gtk": "gtk4:4.24.1@sha256:...",
  "gtk3": "gtk3:3.24.52@sha256:...",
  "pygobject": "pygobject:3.58.1@sha256:...",
  "git": "mingit:2.56.0.windows.1@sha256:..."
}
```

How `from` behaves:

- Each entry becomes a `${<alias>}` placeholder usable in `entrypoint`, `cmd`,
  `path`, and `env`.
- Every `from` reference must be pinned with `@sha256:<manifest-digest>`. If the
  digest is omitted, `quasidock build` fails and prints the resolved
  `windows/amd64` digest to pin. Quasidock resolves the image from the local
  store if present, otherwise from its registry.
- A `from` image's layers are included as lower OCI layers in the built image
  and deployed once per digest on disk, so multiple apps pinning the same base
  image share one unpacked directory and removing another app does not affect
  dependents.
- **Single-level only**: a `from` base image cannot declare `from` images of its
  own (an `sdk` image or `source.image` may have `from` images).
- **Runtime composition**: base images' `env` variables are expanded before the
  app's `env`, and their `path` entries are appended after the app's `path` (in
  sorted alias order). Base-image `uses` port declarations merge with the app's.
- **Permissions do not carry over**: `sandbox`, `network`, `filesystem`,
  `services`, and `host` come only from the app's own `permissions`.

Note: Flatpak-style `extensions`/`extends` points were removed in Quasidock
0.3.0; optional add-ons are wired explicitly via `from` (see
[Legacy Zigsaw differences](#legacy-zigsaw-differences)).

[`scripts/check-pins.sh`](../scripts/check-pins.sh) checks published recipes'
pins against the registry (see [Pins and rebuilding](#pins-and-rebuilding)).

## Images as sources

A stage can copy files directly out of another pinned image using an `image`
source rather than rebuilding them, as [gtk4.json](../recipes/gtk4.json) does
from [gtk4-sdk.json](../recipes/gtk4-sdk.json):

```json
"cleanup": ["/include", "/lib/cmake", "/lib/pkgconfig", "*.a"],
"stages": [
  {
    "name": "gtk",
    "sources": [
      { "image": "gtk4-sdk:4.24.1@sha256:31b88a3d1d0a4530f3a31cf558d7c6c6c58e04a42ced0d7aeb28d0cf501de104" }
    ]
  }
]
```

The source image is pinned like `from`, resolved from the store or registry, and
only its **own** layer files are taken—not any `from` base images it references
(for example, `gtk4-sdk` has `meson`, `llvm`, and `busybox` in `from`, which
are excluded from `gtk4`). Optional `dest` puts the files under a subdirectory,
while `sha256`, `type`, `strip`, and `skip` are disallowed. Rather than
unpacking again, Quasidock streams entries from the deployed image directory
into the new app tree and records the source image digest under `build.images`,
allowing downstream stages/apps to reproduce without rebuilding the SDK.

## `stages`, `sources`, and building from source

Published recipes with a new upstream URL also need a matching entry in
[`scripts/upstream.txt`](../scripts/upstream.txt) (checked by
[tests/upstream.sh](../tests/upstream.sh) in CI; see
[published apps](published-apps.md)).

Each entry in `stages` supports:

- `name`: `1`–`57` characters matching `[A-Za-z0-9._-]`, not starting with `.`,
  and case-insensitively unique across the recipe.
- `sources`: array of source objects. A stage must specify `sources` or `build`.
- `build`: array of commands to run in `B:\src\<stage>`. When `build` is
  omitted, `sources` unpack directly into `B:\prefix`; when `build` is present,
  `sources` unpack into `B:\src\<stage>`.
- `shell`: `"sh"` (default, using `sh.exe` or `busybox.exe sh -c` on tool `PATH`)
  or `"cmd"` (`cmd.exe /d /s /c`). If `build` uses `sh` and no image in `sdk` or
  `from` provides it, add `busybox` to `sdk`.
- `env`: map of environment variables for the stage (requires `build`).
- `network`: boolean (default `false`; requires `build`). When `false`,
  `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` are set to `http://127.0.0.1:9`
  and `NO_PROXY` is cleared during `build`.
- `vendor`: `{ "commands": [...], "dir": "...", "sha256": "..." }` (requires
  `build`; see [Vendor steps](#vendor-steps)).

Each entry in `sources` must set **exactly one** of `url`, `path`, or `image`:

- `url`: HTTP/HTTPS download. Requires `sha256` (64 lowercase hex characters;
  omit once only when discovering an archive hash). Optional `type` (`file`,
  `zip`, `tar`, `tar.gz`, `tar.xz`; `.tgz` maps to `tar.gz`, `.txz` maps to
  `tar.xz`, `.crate` maps to `tar.gz`), `dest` (relative target path), and
  `strip` (archive path components to strip; archives only).
- `path`: file or directory relative to the recipe directory (`"."` allowed; see
  [Projects](#projects)). Directory `path` sources support `skip` using
  `cleanup` pattern syntax (`"/<path>"` anchored from that directory or
  filename globs without `/` or `\`).
- `image`: image reference (`@sha256:...` pinned); `sha256`, `type`, `strip`, and
  `skip` are not allowed.

For example, [sqlite.json](../recipes/sqlite.json) builds `zlib` and then
`sqlite3.exe` with `zig`:

```json
"sdk": {
  "zig": "zig:0.17.0@sha256:93ec2b6b36708ae80ffac87872e70dafaee350cd2d49a95dd2fa5847d0a10449",
  "busybox": "busybox:FRP-6075-g169694ebd@sha256:db9d39727bfdd4d176b5878c0a125daa8a2396ea21e0f01a6a372f53d9d97e5d"
},
"cleanup": ["/include", "/lib"],
"stages": [
  {
    "name": "zlib",
    "sources": [
      { "url": "https://github.com/madler/zlib/releases/download/v1.4/zlib-1.4.tar.xz", "sha256": "...", "strip": 1 }
    ],
    "build": [
      "for f in *.c; do zig cc -target x86_64-windows-gnu -O2 -c \"$f\" -o \"${f%.c}.o\" || exit 1; done",
      "zig ar rcs libz.a *.o",
      "mkdir -p \"$PREFIX/include\" \"$PREFIX/lib\"",
      "cp zlib.h zconf.h \"$PREFIX/include\" && cp libz.a \"$PREFIX/lib\""
    ]
  },
  {
    "name": "sqlite",
    "sources": [
      { "url": "https://sqlite.org/2026/sqlite-amalgamation-3530400.zip", "sha256": "...", "strip": 1 }
    ],
    "build": [
      "mkdir -p \"$PREFIX/bin\"",
      "zig cc -target x86_64-windows-gnu -O2 -s -DSQLITE_HAVE_ZLIB -I\"$PREFIX/include\" -o \"$PREFIX/bin/sqlite3.exe\" shell.c sqlite3.c \"$PREFIX/lib/libz.a\""
    ]
  }
]
```

Key build mechanics:

- **`sdk` and `from` tools**: Both `sdk` and `from` images are mounted on the
  build's `PATH` (each in sorted alias order, `sdk` before `from`), and `build.sdk`
  records `sdk` digests without adding them to the output app image. If an `sdk`
  image declares `from` base images of its own (as `gtk4-sdk` includes `meson`,
  `llvm`, and `busybox`), those follow it in tool order with their `env`
  expanded first. Duplicate image digests are included once, while two different
  versions/digests of the same image ID fail the build.
- **`host` toolchains**: `"host": ["msvc"]` runs `vswhere.exe` / `vcvars64.bat`
  to expose Visual Studio C++ and Windows SDK paths and variables, recording
  `VCToolsVersion` and `WindowsSDKVersion` in `build.host`. Using `host`
  disables the build result cache and incremental work directories.
- **Stage ordering & prefix**: Stages execute sequentially, stopping on the
  first failing command. Everything placed in `$PREFIX` (`B:\prefix`) becomes
  the app layer after `cleanup`. `B:\prefix\bin` sits **first** on `PATH` ahead
  of all `sdk` and `from` tools, so later stages find programs installed by
  earlier stages (such as `glib-compile-resources` during GTK's build).
- **`PKG_CONFIG_PATH` and `CMAKE_PREFIX_PATH`**: `PKG_CONFIG_PATH` starts with
  `B:\prefix\lib\pkgconfig;B:\prefix\share\pkgconfig` followed by any matching
  directories inside `sdk`/`from` tools; `CMAKE_PREFIX_PATH` lists `B:\prefix`
  followed by those tool directories.
- **Tool commands**: SDK recipes package their own executables in `sdk-bin`,
  listed first in the image's `path`. Zig supplies `cc.exe`, `c++.exe`, `ar.exe`,
  `ranlib.exe`, `rc.exe` and `as.exe`; LLVM supplies the corresponding compiler
  and binutils commands. Maven, Gradle, Meson and Composer provide `mvn.exe`,
  `gradle.exe`/`gradle-vendor.exe`, `meson.exe`/`pkg-config.exe`, and
  `composer.exe`. Rust supplies its `dlltool.exe` driver, PHP's SDK supplies
  `php-ext.exe`, and GNU Make supplies `gmake.exe`. These are ordinary image
  files built from [launcher.c](../recipes/sdk-tools/launcher.c) and each SDK's
  `commands.h`; they resolve deployed SDK/runtime paths and preserve arguments,
  working directory and child exit status. Quasidock does not interpret these
  files as alias metadata or generate wrappers at installation/build time.
- **Archivers in shell commands**: spell the command `ar.exe` (including `AR`
  and Cargo's target archiver variables) to use the SDK's native archiver.
  BusyBox's shell treats bare `ar` as its own applet even when another archiver
  is on `PATH`. GNU Make's image sets `BB_OVERRIDE_APPLETS=make` so builds that
  select that SDK use its `make.exe` rather than BusyBox's make applet.

### Build environment

During `build` and `vendor` commands, Quasidock constructs a clean environment
with fresh profile folders, maps the build root to `B:` via `DefineDosDevice`
(serialized across builds using `Local\quasidock-build-drive`; builds fail if
something else already owns `B:`), runs inside a job object that cleans up
leftover child processes, and sets:

- `PREFIX=B:\prefix`
- `SOURCE_DATE_EPOCH=315532800` (1980-01-01)
- `QUASIDOCK_BUILD=1`
- `ZIG_LOCAL_CACHE_DIR=B:\src\<stage>\.zig-cache`
- `ZIG_PROGRESS=off` (avoids a Zig 0.16 console progress hang)
- `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` set to `http://127.0.0.1:9` and
  `NO_PROXY=""` when stage `network` is `false`
- `PKG_CONFIG_PATH`, `CMAKE_PREFIX_PATH`, `PATH` entries (`B:\prefix\bin`,
  tools, and optional `msvc`)
- Stage `env` expanded last

Mapping the build directory to `B:` ensures embedded paths (`__FILE__`, debug
paths) resolve to `B:\src\<stage>\...` across machines. Pass
`quasidock build --keep-build-dir` to keep the build root after completion or
failure for inspection; `quasidock prune` removes kept temporary build roots.

For reproducible binaries with Zig's C compiler:

- Always pass `-target x86_64-windows-gnu` (without it, `zig cc` targets the
  host CPU and Windows build).
- Pass `-s` so Zig omits machine-varying PDB files (Zig also rejects `__DATE__`
  and `__TIME__`).
- Link with an optimization level such as `-O2` or `-Wl,-Brepro` so PE
  timestamps stay deterministic.

**Build result cache**: Quasidock hashes `quasidock.exe`, the recipe bytes,
local source files, and directory snapshots. Rebuilding unchanged inputs reuses
the cached manifest digest (`--verbose` reports the hit); `--rebuild`,
`--clean`, or `--keep-build-dir` skips reading the cache, and `host` recipes
never read or write it.

**Tool caches (`${cache}`)**: In builds, `${cache}` resolves to
`B:\cache\<tool id>`, persisted in the store between builds (`cache\tools\<id>`)
and moved onto `B:` for each run—for example `"ZIG_GLOBAL_CACHE_DIR": "${cache}"`
in [zig.json](../recipes/zig.json) keeps Zig's compiled C runtime across builds.
Only content-addressed, reproducible tool caches belong in `${cache}`; Zig's
per-project `.zig-cache` stays in `B:\src\<stage>\.zig-cache` because Zigsaw
[iteration 36](../legacy/iteration-36.md) found that caching compiled objects in
`${cache}` suppressed compiler warnings on subsequent `cc-rs` flag probes and
broke reproducibility in `cargo-c` ([findings](findings.md)).

## Vendor steps

Package managers such as Cargo, Go, Composer, Pip, and `zig build --fetch` need
network access to download locked dependencies before an offline `build` stage
runs:

```json
"vendor": {
  "commands": ["cargo vendor --locked vendor"],
  "dir": "vendor",
  "sha256": "c58cbe1a06b9fa52..."
}
```

- `vendor` requires `build` on the stage.
- `vendor.commands` run **with network access** inside `B:\src\<stage>` after
  downloads finish and before any stage runs `build`, seeing only the stage's own
  sources and build tools (`sdk`/`from`/`host`), not files installed by earlier
  stages into `$PREFIX`.
- `vendor.dir` (a relative path inside the stage directory) is packed into a
  canonical tar (sorted, deterministic timestamps and owners) and cached under
  `cache\downloads\<sha256>`.
- Omit `sha256` on the first build to make `quasidock build` print the computed
  digest to pin; a mismatched digest fails the build and shows expected vs got.
- If `vendor.commands` fail and `sha256` is pinned, Quasidock falls back to
  fetching the source artifact pushed next to the image (`push --sources`) and
  verifying its sha256.
- After vendoring (or fallback), Quasidock wipes and re-unpacks the stage's
  sources, resets build profile folders, overlays the verified `dir`, records the
  digest in `build.vendor`, and runs `build` offline.

## Pins and rebuilding

All `from`, `sdk`, and `source.image` references must be pinned with
`@sha256:<manifest-digest>`. If an unpinned reference is passed, `quasidock build`
fails and prints the resolved `windows/amd64` manifest digest to pin.

Because downstream recipes pin upstream toolchains and base images by digest,
rebuilding an upstream recipe requires rebuilding and re-pinning consumers in
dependency order:

- [scripts/published-recipes.txt](../scripts/published-recipes.txt) lists
  standalone published recipes.
- [scripts/published-groups.txt](../scripts/published-groups.txt) lists grouped
  recipe build sets.
- [scripts/rebuild.sh](../scripts/rebuild.sh) builds named recipes and everything
  that pins them in topological order:

```bash
scripts/rebuild.sh --plan go.json zot.json     # print build order without building
QUASIDOCK_HOME=path\to\store QUASIDOCK=path\to\quasidock.exe scripts/rebuild.sh go.json zot.json
QUASIDOCK_HOME=path\to\store2 QUASIDOCK=path\to\quasidock.exe scripts/rebuild.sh --no-pin go.json zot.json
```

`QUASIDOCK_HOME` is required so rebuilds do not mutate your default store.
Using a frozen `quasidock.exe` binary keeps cache keys valid across the run.
`rebuild.sh` updates pins in `recipes/` (excluding `recipes/no-publish/`, which
it skips and lists at the end), stops on the first failure leaving the log
tail, and prints the [scripts/publish.sh](../scripts/publish.sh) command to run.
Running `--no-pin` in a second store lets you verify identical output digests
before publishing.

[`scripts/check-pins.sh`](../scripts/check-pins.sh) verifies anonymously
against the registry (`ghcr.io/quasidock/quasidock` or `QUASIDOCK_REGISTRY`) that every
pinned digest and published version tag exists (plus `.sig` signatures on the
default registry), failing on missing artifacts and warning when a `from` base
image pin's layers no longer match its version tag (older `sdk` pins do not
warn).

## Projects

A recipe can live at the root of the project it builds as `quasidock.json`.
Running `quasidock build` with no argument builds `quasidock.json` in the
current directory; `quasidock build <dir>` builds `<dir>\quasidock.json`. Parent
directories are not searched, and a directory without `quasidock.json` fails
with `has no quasidock.json; give a recipe`.

Project recipes use a directory `path` source (for example `"path": "."`, with
or without `build` commands):

```json
"sdk": {
  "busybox": "busybox:FRP-6075-g169694ebd@sha256:..."
},
"stages": [
  {
    "name": "app",
    "sources": [
      { "path": ".", "skip": ["/.github", "/docs"] }
    ],
    "build": [
      "make",
      "cp out/app.exe \"$PREFIX/\""
    ]
  }
]
```

A directory `path` source includes the files Git would commit:

- **`.gitignore` rules apply**: `.gitignore` files inside the directory, below
  it, and above it up to the repository top (where `.git` is) are evaluated
  with Git syntax: `#` comments, `!` negation (without re-entering an already
  ignored directory), trailing `/` for directories only, leading or middle `/`
  anchoring to that `.gitignore`'s directory, and `*`, `?`, `[...]`, `**`
  wildcards. Matching is case-insensitive (`core.ignorecase`). Machine-local
  global excludes and `.git/info/exclude` are ignored.
- **`.git` and empty directories stay out**: `.git` directories or worktree/submodule
  files are excluded, empty directories are omitted, and nested `.git`
  repositories (such as submodules) apply only their own `.gitignore` rules.
- **`skip` patterns** filter further using `cleanup` pattern syntax (`"/docs"`,
  `"*.log"`).
- **Errors**: symlinks or junctions fail the build unless skipped (naming the
  link), and an empty file set or a directory ignored by an ancestor
  `.gitignore` also fails. `dest` places files under a subdirectory; `sha256`,
  `type`, and `strip` are not allowed on directory sources. Use `.gitattributes`
  to normalize line endings across machines.

Directory files are hashed canonically into a deterministic tar digest recorded
under `build.sources` (never cached in `cache\downloads` and not uploaded by
`push --sources`). When `build` commands are present, files are copied into
`B:\src\<stage>` so the project checkout is never modified, and re-hashed after
copying to catch concurrent edits during the build.

### Incremental builds

A stage that has `build` commands and at least one directory `path` source—and
does not use `vendor` or `host`—retains its stage work directory between builds
under `cache\work\<key>\<stage>` (where `<key>` is a 16-hex hash of the
lowercased recipe path and app `id`, keeping separate checkouts independent).
Build artifacts such as `.zig-cache`, CMake/Meson build directories, and Cargo
`target/` remain intact across rebuilds:

- **Incremental sync**: files whose contents changed or were modified in place
  by an earlier build command (such as `sed -i`) are rewritten from the project
  checkout, files removed from the project are deleted from work, and unchanged
  files keep their existing timestamps so `make`, `ninja`, and `cargo` rebuild
  only what changed.
- **Clean resets**: the work directory starts afresh whenever `quasidock.exe`,
  the recipe file, or any non-project local input changes, or when `--clean` or
  `--keep-build-dir` is passed (`--clean` also forces a fresh rebuild).
- **Fresh elsewhere**: `B:\prefix`, `B:\home`, and `B:\data` start clean on
  every build, and stages without `build`, stages using `vendor`, recipes using
  `host: ["msvc"]`, and recipes with only pinned inputs never retain a work
  directory.
- **Storage and pruning**: the work directory moves into `B:` during the build
  and back to `cache\work\<key>\<stage>` afterward (including on failed builds),
  unless `--keep-build-dir` leaves it inside the kept build root.
  `quasidock prune` preserves project work directories;
  `quasidock prune --downloads` removes them.

Whether an incremental build matches a `--clean` build depends on the project's
build tool ([tests/build.sh](../tests/build.sh) verifies `make` incremental
rebuilds and `--clean` digest parity). Rebuild a project at any time by running
`quasidock build [dir]` again.

## Toolchains and ecosystems

### Zig

[recipes/zig.json](../recipes/zig.json) packages Zig 0.17.0 (which bundles
Clang 22, LLD, and mingw-w64 headers and import libraries) with
`"entrypoint": "zig.exe"`, `"permissions": { "sandbox": "soft", "filesystem":
["cwd"] }`, and default `path` (`["."]`). Running toolchains under `soft` keeps
compiled binaries from inheriting a low-integrity label (which would force
Windows to run produced executables at low integrity outside the container) and
avoids AppContainer `self-path-dos` failures in `zig env` on some Windows builds
(see [tests/zls.sh](../tests/zls.sh)).

The image sets `ZIG_GLOBAL_CACHE_DIR` to `${cache}`. During builds, Quasidock
maps `${cache}` to `B:\cache\zig` and moves it back to the store's
`cache\tools\zig` afterward, so Zig's compiled C and POSIX runtime objects stay
warm across builds while remaining isolated from container data (`${data}\zig`
at runtime). Note that Zigsaw's old `"aliases"` field (which generated thin
wrapper executables like `cc.exe`, `c++.exe`, `ar.exe`, `rc.exe`, etc.) was
removed in Quasidock; `recipes/zig.json` still exports the compiler environment
variables below, while current C-based recipes (such as
[recipes/sqlite.json](../recipes/sqlite.json)) invoke `zig cc` and `zig ar`
directly:

```json
"env": {
  "ZIG_GLOBAL_CACHE_DIR": "${cache}",
  "CC": "cc",
  "CXX": "c++",
  "RC": "rc",
  "LDFLAGS": "-s -Wl,-Brepro",
  "CGO_LDFLAGS": "-s -Wl,-Brepro",
  "CC_x86_64_pc_windows_gnu": "cc",
  "CXX_x86_64_pc_windows_gnu": "c++",
  "AR_x86_64_pc_windows_gnu": "ar",
  "CRATE_CC_NO_DEFAULTS": "1",
  "CFLAGS_x86_64_pc_windows_gnu": "-O3 -ffunction-sections -fdata-sections",
  "CXXFLAGS_x86_64_pc_windows_gnu": "-O3 -ffunction-sections -fdata-sections"
}
```

[recipes/zls.json](../recipes/zls.json) builds the Zig Language Server
(0.17.0) on `"from": { "zig": "zig:..." }` using a `vendor` stage (`zig build
--fetch`) followed by `zig build -Doptimize=ReleaseSafe -Dstrip
-Dversion-string=0.17.0`. At runtime, `zls` sets `"path": ["${zig}"]` and
`"permissions": { "sandbox": "soft", "filesystem": ["cwd"] }` so ZLS can invoke
`zig`, inspect the standard library, and keep its caches and logs inside its
container data directory ([tests/zls.sh](../tests/zls.sh)).

### Clang (llvm-mingw)

[recipes/llvm-mingw.json](../recipes/llvm-mingw.json) packages the UCRT x86\_64
`llvm-mingw` toolchain (`20261020`, `"layer": "estargz"`, `"entrypoint":
"bin/clang.exe"`, `"path": ["bin"]`, `"permissions": { "sandbox": "soft" }`),
setting `CC="cc"`, `CXX="c++"`, `RC="windres"`, and `LDFLAGS="-s"` so downstream
builds can swap `zig` for `llvm-mingw` without changing build scripts.

The recipe removes shared runtime DLLs and `.dll.a` import libraries for
`libc++`, `libunwind`, and `libwinpthread`, leaving static libraries only.
Programs and DLLs built with `llvm-mingw` therefore need no compiler runtime
DLLs at runtime; cross-DLL C++ exceptions, RTTI, `std::function`, and
`thread_local` are verified by [tests/build/xdll/xdll.json.in](../tests/build/xdll/xdll.json.in)
and [tests/build.sh](../tests/build.sh). To keep PE/COFF outputs reproducible,
the recipe appends `-mno-incremental-linker-compatible` and
`-Wl,--no-insert-timestamp` to `bin/mingw32-common.cfg`.

### CMake

[recipes/cmake.json](../recipes/cmake.json) bundles CMake 4.4.4 with Ninja
1.13.2, sets `"CMAKE_GENERATOR": "Ninja"`, `"entrypoint": "bin/cmake.exe"`,
`"path": ["bin"]`, and replaces `share/cmake-4.4/Modules/Platform/WindowsPaths.cmake`
with [recipes/cmake/WindowsPaths.cmake](../recipes/cmake/WindowsPaths.cmake).
That override clears host `Program Files`, package registry lookups
(`CMAKE_FIND_USE_PACKAGE_REGISTRY=OFF`,
`CMAKE_FIND_USE_SYSTEM_PACKAGE_REGISTRY=OFF`), and registry Python probes so
`find_package` and `find_program` only see `$PREFIX` and the image's SDKs
(verified in [tests/build.sh](../tests/build.sh)).

During builds, Quasidock sets `CMAKE_PREFIX_PATH` to `B:\prefix` plus every SDK
or base image directory that provides `lib\pkgconfig` or `share\pkgconfig`.
Recipes typically install into `"$PREFIX"` and strip development headers and
import libraries from final app images with `"cleanup": ["/include", "/lib"]`,
as in [recipes/zstd.json](../recipes/zstd.json) and
[tests/build/cmake/hello.json.in](../tests/build/cmake/hello.json.in):

```json
{
  "id": "test.build.cmake",
  "version": "1",
  "entrypoint": "bin/hello.exe",
  "sdk": {
    "cmake": "cmake:4.4.4@sha256:...",
    "zig": "zig:0.17.0@sha256:...",
    "busybox": "busybox:FRP-6075-g169694ebd@sha256:..."
  },
  "cleanup": ["/include", "/lib"],
  "stages": [
    {
      "name": "hello",
      "sources": [
        { "path": "CMakeLists.txt" },
        { "path": "main.c" }
      ],
      "build": [
        "cmake -S . -B out -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=\"$PREFIX\"",
        "cmake --build out",
        "cmake --install out"
      ]
    }
  ]
}
```

### Meson

[recipes/meson.json](../recipes/meson.json) bundles embeddable Python 3.14.7,
Meson 1.12.1, Ninja 1.13.2, and `pkgconf` 3.0.7 (built statically with `zig`),
with `"entrypoint": ["python.exe", "${app}\\meson\\meson.py"]`.

Meson normally resolves `os.path.realpath()` on build directories, which turns
paths on the temporary `B:` subst drive into random store paths such as
`...\quasidock\tmp\build-...\src` that leak into `__FILE__` and debug data. The
recipe strips `os.path.realpath(` calls from `utils/universal.py`, `msetup.py`,
and `mconf.py` so `B:\src\...` stays stable. Quasidock's builder automatically
populates `PKG_CONFIG_PATH` with `B:\prefix\lib\pkgconfig;B:\prefix\share\pkgconfig`
followed by every SDK/base image's `pkgconfig` directories, allowing multi-stage
Meson builds to discover earlier stages and SDK libraries via `dependency()` (see
[tests/build/meson/hello.json.in](../tests/build/meson/hello.json.in)).

### GNU make

[recipes/make.json](../recipes/make.json) builds GNU make 4.4.1 with `zig` and
sets `"entrypoint": "bin/make.exe"` and `"path": ["bin"]`. Alongside
`make.exe`, it compiles [recipes/make/sh.c](../recipes/make/sh.c) into
`bin/sh.exe`, which launches `busybox.exe sh` inside a `KILL_ON_JOB_CLOSE` job
object, and applies [recipes/make/posix-shell.patch](../recipes/make/posix-shell.patch)
so GNU make does not bypass `sh` with Windows' internal command parser when
recipes use POSIX builtins (`echo`, `mkdir -p`, `for` loops; see
[tests/build/make/make.json.in](../tests/build/make/make.json.in)).

Many simpler autotools builds in `recipes/` use BusyBox's built-in `pmake`
(`make`) instead, paired with [recipes/gtk4-sdk/autotools-mtimes.sh](../recipes/gtk4-sdk/autotools-mtimes.sh)
(or [recipes/gnumeric/pdpmake.sh](../recipes/gnumeric/pdpmake.sh)) to normalize
tarball modification times and replace GNU make pattern rules.

### GTK

The GTK stack is split into base SDK, full SDK, and slim runtime layers built
with `meson`, `llvm-mingw`, and `busybox` (plus `cmake`, `perl`, `rust`, and
`cargo-c` in the SDKs):

- **[recipes/gtk4-sdk-base.json](../recipes/gtk4-sdk-base.json)** builds the
  core stack up to GTK 4.24.1 (`zlib`, `glib`, `python` 3.14.10 NuGet +
  `setuptools` + `winflexbison`, `gobject-introspection` 1.86.0, `fribidi`,
  `pixman`, `libpng`, `libjpeg-turbo`, `libtiff`, `harfbuzz` 14.6.0, `pango`
  1.58.2, `cairo` 1.18.6, `gdk-pixbuf` 2.43.5, `graphene`, `libepoxy`, and
  `DirectX-Headers`). Helper patches in `recipes/gtk4-sdk/` address subst-drive
  scanning ([gobject-introspection.patch](../recipes/gtk4-sdk/gobject-introspection.patch)),
  shebang wrappers ([python-script.c](../recipes/gtk4-sdk/python-script.c)),
  `libtool` `--enable-auto-import` detection ([autotools-ld.sh](../recipes/gtk4-sdk/autotools-ld.sh)),
  and a focus-removal crash in GTK's Windows IME handler
  ([gtk-ime-filter.patch](../recipes/gtk4-sdk/gtk-ime-filter.patch)).
- **[recipes/gtk4-sdk.json](../recipes/gtk4-sdk.json)** imports
  `gtk4-sdk-base` as an `"image"` source and adds `hicolor-icon-theme`,
  `adwaita-icon-theme` 51.0, `libadwaita` 1.10.0, `openssl` 3.5.9 (via
  [openssl-unixspec.pm](../recipes/gtk4-sdk/openssl-unixspec.pm)), `sqlite`,
  `nghttp2`, `libpsl`, `glib-networking`, `libsoup` 3.8.1, `freetype`,
  `libxml2`, `librsvg` 2.63.4, `hunspell`, `enchant` 2.8.21
  ([enchant.ordering](../recipes/gtk4-sdk/enchant.ordering) prefers Windows'
  WinSpell before Hunspell), and the `en_US` dictionary. Both `gtk4-sdk`
  layers rewrite `.pc` prefixes to `${pcfiledir}/../..`.
- **[recipes/gtk4.json](../recipes/gtk4.json)** turns `gtk4-sdk` into the
  slim runtime image (`"entrypoint": "bin/gtk4-demo.exe"`), stripping headers,
  static/import libs, `.pc`/`.gir` files, Python, and gettext developer tools,
  generating `.ico` icons via [svg2ico.sh](../recipes/gtk4-sdk/svg2ico.sh), and
  configuring `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`,
  `XDG_CACHE_HOME`, `XDG_DATA_DIRS`, and `GSETTINGS_BACKEND=keyfile` so GLib
  and GTK keep all state in `${data}` rather than the host registry or
  `AppData`.

Note that the two legacy dictionary extension images (`gtk4-hunspell-fr`,
`gtk4-hunspell-vi`) were removed in Quasidock (commit `df24cf4`) when extension
points were dropped. Apps can run runtime tools such as `gtk4-query-settings` or
`pango-view` using `--entrypoint` (see [tests/gtk.sh](../tests/gtk.sh)):

```sh
quasidock run --rm --entrypoint=gtk4-query-settings gtk4
```

Downstream GTK 4 applications build with `"from": { "gtk": "gtk4:..." }` and
`"sdk": { "gtksdk": "gtk4-sdk:...", ... }`:

- **[recipes/gnome-text-editor.json](../recipes/gnome-text-editor.json)**
  (51.0) builds `gtksourceview` 5.22.0, `libspelling` 0.4.10 (linked with
  Windows `-licu` and a stub `icu-uloc.h` staged at `zigsaw/unicode/uloc.h`),
  `editorconfig-core-c`, and GNOME Text Editor.
- **[recipes/rnote.json](../recipes/rnote.json)** (0.14.2) uses `rust` and
  `gtk4-sdk` (aliased as `toolkit`), replacing upstream's Unix-dependent Cargo
  wrapper with
  [cargo_build.py](../recipes/rnote/cargo_build.py), linking with Rust's
  self-contained `x86_64-w64-mingw32-gcc.exe` (`libgcc_eh`), disabling
  upstream's relative `XDG_DATA_DIRS` override ([data-dirs.patch](../recipes/rnote/data-dirs.patch)),
  registering bundled fonts per-thread on Rayon workers ([fonts.patch](../recipes/rnote/fonts.patch),
  [fonts.rs](../recipes/rnote/fonts.rs)), and falling back to in-place saves if
  a sandbox only grants write access to an opened file ([save-in-place.patch](../recipes/rnote/save-in-place.patch)).
  SDK aliases are sorted: `rust` must precede `toolkit` so Rust's GNU `dlltool`
  wins over the GTK SDK's LLVM tool. LLVM-generated `raw-dylib` import libraries
  linked by GNU `ld` leave Windows imports unbound and crash before `--help`.

**GTK 3** is packaged as an add-on layer on top of `gtk4`:

- **[recipes/gtk3-sdk.json](../recipes/gtk3-sdk.json)** (`3.24.52`, `"sdk": {
  "gtksdk": ... }`) builds `at-spi2-core` (`atk_only`), GTK 3.24.52 (with
  `-fmacro-prefix-map=$sdk=gtksdk`), and `adwaita-icon-theme-legacy` 46.2.
- **[recipes/gtk3.json](../recipes/gtk3.json)** strips dev artifacts and exports
  `GSETTINGS_SCHEMA_DIR` and `XDG_DATA_DIRS` for GTK 3 runtimes. Apps such as
  [recipes/gtk3demo.json](../recipes/gtk3demo.json) and
  [recipes/gnumeric.json](../recipes/gnumeric.json) (`1.12.62`, building
  `libxslt`, `libgsf`, `intltool`, `goffice`, and Gnumeric with
  [pdpmake.sh](../recipes/gnumeric/pdpmake.sh) and
  [png2ico.sh](../recipes/gtk3/png2ico.sh)) declare both `"gtk"` and `"gtk3"`
  in `"from"`. Run `ssconvert` via `--entrypoint=ssconvert gnumeric ...` (see
  [tests/gtk3.sh](../tests/gtk3.sh)).
- **PyGObject**: [recipes/pygobject-sdk.json](../recipes/pygobject-sdk.json)
  (3.58.1) and [recipes/pygobject.json](../recipes/pygobject.json) package Python
  3.14 with `pycairo` 1.29.2, `pygobject`, and synthetic `Lib/pkgconfig/python*.pc`
  files so Python apps can introspect GTK 3 and GTK 4 via typelibs.

### Qt

Like GTK, Qt 6.12.0 is split into an eStargz SDK base, full SDK, and slim
runtime using `cmake`, `llvm-mingw`, and `busybox` in `"from"`:

- **[recipes/qt6-sdk-base.json](../recipes/qt6-sdk-base.json)** builds `qtbase`
  (via [qtbase.sh](../recipes/qt6-sdk/qtbase.sh), adding `#include "qspan.h"`,
  quoting `RT_MANIFEST` paths, setting `-DCMAKE_SYSTEM_VERSION=10.0.26300`, and
  applying [qlocalserver-appcontainer.patch](../recipes/qt6-sdk/qlocalserver-appcontainer.patch)
  so `QLocalServer` / `QLocalSocket` use `\\.\pipe\LOCAL\` inside AppContainers),
  plus `qtsvg`, `qtimageformats`, and `qtshadertools`.
- **[recipes/qt6-sdk.json](../recipes/qt6-sdk.json)** (`"layer": "estargz"`,
  with `"prefetch"` for startup-hot files) adds `qtdeclarative` (unity build +
  second pass + embeddable Python 3.14.7), `qttools`, and `qttranslations`, and
  verifies `designer.exe` is built as a Windows GUI binary. Both SDKs normalize
  `.pc` prefixes to `${pcfiledir}/../..`.
- **[recipes/qt6.json](../recipes/qt6.json)** (`"layer": "estargz"`,
  `"entrypoint": "bin/designer.exe"`, `"registry": ["Software\\QtProject"]`)
  imports `qt6-sdk`, drops dev tools/headers/libs, and writes `bin/qt.conf`
  (`Prefix = ..`) so Qt locates plugins, QML modules, and translations
  relocatably next to `Qt6Core.dll` rather than `B:\prefix`. Other Qt tools run
  via `--entrypoint`, e.g. `quasidock run --rm --entrypoint=qml qt6 ran.qml`
  ([tests/qt.sh](../tests/qt.sh)).

Downstream Qt applications use `"from": { "qt": "qt6:..." }` and `"sdk": {
"qtsdk": "qt6-sdk:..." }`:

- **[recipes/flameshot.json](../recipes/flameshot.json)** (14.0.0) bundles
  `QHotkey` via `FETCHCONTENT_SOURCE_DIR_QHOTKEY`, disables update checks and
  Imgur uploads (`-DDISABLE_UPDATE_CHECKER=ON -DENABLE_IMGUR=OFF`), and cleans
  up `/bin/flameshot-cli.exe`.
- **[recipes/notes.json](../recipes/notes.json)** (2.3.1, `"entrypoint":
  "bin/Notes.exe"`) adds `CorePrivate GuiPrivate WidgetsPrivate` for Qt 6.12
  and sets `-DUPDATE_CHECKER=OFF`.

### Python

[recipes/python.json](../recipes/python.json) packages Python 3.14.10
(embeddable x64 zip) with `"permissions": { "sandbox": "soft", "network":
true, "filesystem": ["cwd"] }`. It includes
[recipes/python/python314._pth](../recipes/python/python314._pth) (`import site`)
and [recipes/pygobject/sitecustomize.py](../recipes/pygobject/sitecustomize.py),
which hooks `os.mkdir` (turning `0o700` into `0o777` inside AppContainers so
`tempfile.mkdtemp` directories inherit grant ACLs instead of locking the
container out) and moves `multiprocessing.connection` named pipes to
`\\.\pipe\LOCAL\`.

Python applications vendor their PyPI dependencies offline using `vendor`
stages and pip requirements files:

- **[recipes/gaphor.json](../recipes/gaphor.json)** (3.3.2, `"from": { "gtk":
  "gtk4:...", "pygobject": "pygobject:..." }`) builds `gtksourceview` 5.22.0
  and `graphviz` 16.1.0 (`dot`), vendors wheels from
  [build-requirements.txt](../recipes/gaphor/build-requirements.txt) (`poetry-core==2.5.0`)
  and [requirements.txt](../recipes/gaphor/requirements.txt) (`--require-hashes`),
  and installs with `pip install --no-index --no-deps --no-build-isolation`.
- **[recipes/meld.json](../recipes/meld.json)** (3.24.1) runs on `"from": {
  "gtk": "gtk4:...", "gtk3": "gtk3:...", "pygobject": "pygobject:...",
  "git": "mingit:..." }`, combining GTK 3/4 typelibs, PyGObject's `pythonw.exe`,
  and MinGit on `PATH`.

### Rust

[recipes/rust.json](../recipes/rust.json) installs Rust 1.99.0 (`x86_64-pc-windows-gnu`,
`"entrypoint": "bin/cargo.exe"`, `"path": ["bin", "${data}\\cargo\\bin"]`),
keeping `CARGO_HOME` in `${data}\cargo` and setting
`CARGO_PROFILE_RELEASE_STRIP=symbols`. Because `rust` includes GNU `dlltool` in
its self-contained toolchain (avoiding unbound `raw-dylib` imports seen with
`llvm-dlltool`; see [legacy/iteration-12.md](../legacy/iteration-12.md)), built
executables bind all Windows imports under `quasidock-imports.exe` (`go run
./scripts/build.go imports`, [tests/build.sh](../tests/build.sh)).

- **[recipes/cargo-c.json](../recipes/cargo-c.json)** (0.10.25) vendors crates
  from crates.io and builds `cargo-c` (used by `gtk4-sdk` to build `librsvg`).
- **[recipes/ripgrep.json](../recipes/ripgrep.json)** (15.2.0) builds with
  `--profile release-lto` and vendored crates.
- **[recipes/bat.json](../recipes/bat.json)** (0.26.1) vendors crates and builds
  `bat.exe` with `zig`'s `cc`.

When pairing `rust` with `llvm-mingw` instead of `zig`, set
`CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER` to Rust's self-contained
`x86_64-w64-mingw32-gcc.exe` (as in [recipes/rnote.json](../recipes/rnote.json))
so GNU `ld` links `libgcc_eh` cleanly; see [tests/build.sh](../tests/build.sh).

### Go

[recipes/go.json](../recipes/go.json) packages Go 1.27.3 with
`GOPATH="${data}\\go"`, `GOCACHE="${cache}"` (persisted across builds in
`cache\tools\go`), `GOTOOLCHAIN="local"`, and `GOFLAGS="-trimpath -modcacherw"`
so source paths inside binaries reflect module paths rather than temporary
`B:\src\...` directories. Pure Go builds need no C toolchain; `cgo` builds pair
`go` with `zig` (see [tests/build/cgo/hello.json.in](../tests/build/cgo/hello.json.in)),
passing `-ldflags "-buildid="` so Go does not hash temp filenames from `zig cc`
version probes.

Go recipes (`vendor` stage running `go mod vendor` / `go mod download`, offline
`build` stage) include:

- **[recipes/fzf.json](../recipes/fzf.json)** (0.74.5) and
  **[recipes/zot.json](../recipes/zot.json)** (2.1.23, built with
  `GOEXPERIMENT=jsonv2` and a pruned vendor cache).
- **[recipes/oras.json](../recipes/oras.json)** (1.3.6).
- **[recipes/skopeo.json](../recipes/skopeo.json)** (1.24.1), which bundles
  [recipes/skopeo/policy.json](../recipes/skopeo/policy.json)
  (`insecureAcceptAnything`) as its default signature policy. Because Quasidock
  images use `application/vnd.quasidock.app.config.v2+json` rather than OCI's
  standard config media type, `skopeo` treats them as OCI artifacts (`skopeo
  inspect --raw` works; see [tests/oci.sh](../tests/oci.sh)).
- **[recipes/cosign.json](../recipes/cosign.json)** (3.1.4), used directly by
  [scripts/sign-images.sh](../scripts/sign-images.sh) and verified offline in
  [tests/oci.sh](../tests/oci.sh).

### .NET

Because Microsoft's license for Windows .NET binaries does not permit
redistribution, .NET recipes live under `recipes/no-publish/` and are built
locally from Microsoft's zip archives:

- **[recipes/no-publish/dotnet-sdk.json](../recipes/no-publish/dotnet-sdk.json)**
  (10.0.402) and **[recipes/no-publish/dotnet.json](../recipes/no-publish/dotnet.json)**
  (10.0.13) create the SDK (`dotnet-sdk`) and shared runtime (`dotnet`,
  extracted from the SDK with `DOTNET_ROOT="${app}"` so framework-dependent apps
  never consult a machine-wide .NET install; includes `Microsoft.NETCore.App`,
  `Microsoft.AspNetCore.App`, and `Microsoft.WindowsDesktop.App` for WinForms
  and WPF). See [tests/build.sh](../tests/build.sh).
- **[recipes/no-publish/pinta.json](../recipes/no-publish/pinta.json)** (3.1.2)
  runs on `"from": { "dotnet": "...", "gtk": "gtk4:..." }`, builds `net10.0`,
  and grants `"filesystem": ["cwd"]`.
- **[recipes/no-publish/files.json](../recipes/no-publish/files.json)** (Files
  4.3, named `"Files - Zigsaw build"` via [name.patch](../recipes/no-publish/files/name.patch))
  patches Files with [unpackaged.patch](../recipes/no-publish/files/unpackaged.patch)
  to publish framework-dependent (`WindowsPackageType=None`,
  `WindowsAppSDKSelfContained=true`), copies `Package.appxmanifest` and `Assets`,
  keeps settings in `"registry": ["Software\\Files Community"]`, and requests
  `"sandbox": "soft"` because Windows storage APIs reject unpackaged WinUI 3
  enumeration under `low` or `appcontainer` (see [findings.md](findings.md) and
  [tests/files.sh](../tests/files.sh)).

### Java

Java toolchains package Eclipse Temurin OpenJDK 25.0.5\_8:

- **[recipes/java-sdk.json](../recipes/java-sdk.json)** (`java-sdk`) packages the
  full JDK, and **[recipes/java.json](../recipes/java.json)** (`java`) creates
  the slim JRE (`java.exe` / `javaw.exe`, no `javac.exe`). Both set
  `JAVA_TOOL_OPTIONS` with `-Duser.home=${data}\home` (since Java uses
  `SHGetKnownFolderPath` rather than `USERPROFILE`) and
  `-Dsun.font.layout.ffm=false` (so Swing text layout avoids resolving `System32`
  via `toRealPath`, which fails in AppContainers; see [tests/build.sh](../tests/build.sh)).
- **[recipes/maven.json](../recipes/maven.json)** (`3.10.0`, built on `"from": {
  "java": "java-sdk:..." }`) vendors project dependencies and plugins into a
  local Maven repository, stripping `maven-metadata*`, `_remote.repositories`,
  `*.lastUpdated`, and resolver `.meta` files before running offline (see
  [tests/build/maven/hello.json.in](../tests/build/maven/hello.json.in)).

#### Gradle

[recipes/gradle.json](../recipes/gradle.json) builds the `gradle` image (`9.8.1`)
on `"java"` (`"from": { "java": "java-sdk:..." }`), invoking Java with
`-Dorg.gradle.daemon=false` in its `entrypoint` and packaging two helper files
for reproducible offline builds:

- [recipes/gradle/zigsaw-offline.gradle](../recipes/gradle/zigsaw-offline.gradle)
  (staged at `init.d/zigsaw-offline.gradle`): when the `ZIGSAW_GRADLE_REPO`
  environment variable names a directory, it redirects every
  `MavenArtifactRepository` declared in settings (`buildscript`,
  `pluginManagement`, `dependencyResolutionManagement`) and projects
  (`buildscript`, `repositories`) to that directory's `file:` URI, and fails
  with `GradleException` if a non-Maven repository is declared.
- [recipes/gradle/GradleVendor.java](../recipes/gradle/GradleVendor.java)
  (staged at `zigsaw/GradleVendor.java`): copies downloaded artifacts from a
  Gradle home's `caches/modules-2/files-2.1/<group>/<module>/<version>/<sha1>/<file>`
  layout into a plain Maven repository layout
  `<group-dirs>/<module>/<version>/<file>`, stripping non-deterministic Gradle
  cache metadata so a stage's `vendor` directory hashes reproducibly for
  offline `ZIGSAW_GRADLE_REPO="$PWD/vendor" gradle --offline` builds.

Java application recipes targeting `"java"` (or `"java-sdk"` for Scene Builder)
include:

- **[recipes/pdfbox.json](../recipes/pdfbox.json)** (3.0.8, Maven build) and
  **[recipes/plantuml.json](../recipes/plantuml.json)** (1.2026.9, Gradle build;
  extracts its embedded Graphviz `dot` into `${data}` temp at runtime).
- **[recipes/logisim-evolution.json](../recipes/logisim-evolution.json)**
  (5.0.0, Gradle build) and **[recipes/scenebuilder.json](../recipes/scenebuilder.json)**
  (26.0.0, Maven build on `"java-sdk"` + bundled OpenJFX 26.0.2 DLLs in `bin` so
  JavaFX does not unpack DLLs into `user.home`). Both persist preferences under
  `HKCU\Software\JavaSoft\Prefs` in container registry hives; see
  [tests/java.sh](../tests/java.sh).

### PHP

- **[recipes/php-sdk.json](../recipes/php-sdk.json)** (`8.5.12-1`, `"from": {
  "zig": "...", "busybox": "...", "node": "..." }`) pairs upstream MSVC-built
  PHP with PHP development headers, `.lib` import libraries, `php.net`
  dependency `deps/`, and the `php-ext` extension builder under `dev/zigsaw/`
  ([php-ext.php](../recipes/php-sdk/php-ext.php),
  [config-w32.js](../recipes/php-sdk/config-w32.js),
  [zig-php.h](../recipes/php-sdk/zig-php.h),
  [zig-php.c](../recipes/php-sdk/zig-php.c)).
- **[recipes/php.json](../recipes/php.json)** copies the runtime files from
  `php-sdk`, strips `/dev`, and ships [recipes/php-sdk/php.ini](../recipes/php-sdk/php.ini)
  with `extension_dir = "${PHPRC}\ext"`, standard built-in extensions enabled,
  and `curl.cainfo = "${SSL_CERT_FILE}"` pointing at Quasidock's trusted root
  certificate bundle (`${roots}`).

`php-ext` builds C extensions with `zig`'s Clang against Microsoft-compiled
`php8.dll`: `config-w32.js` executes upstream `config.w32` scripts under Node,
`zig-php.h` aligns MSVC macros, `__vectorcall` (`ZEND_FASTCALL`), and MinGW
stat/time headers, and `zig-php.c` supplies `__imp_zend_string_equal_val@@16`.
Unlike legacy Zigsaw, Quasidock removed extension points and the 7 published
extension recipes (`php-apcu`, `php-igbinary`, `php-redis`, `php-ssh2`,
`php-xdebug`, `php-yaml`, `php-zstd`) in commit `df24cf4`. Extensions built with
`php-ext` (see [tests/build/php/zprobe.json.in](../tests/build/php/zprobe.json.in))
are instead composed into an app recipe's `"from"` alongside `"php"`, using
`PHP_INI_SCAN_DIR` to load their `conf.d` directories (as in
[tests/build/php/app.json.in](../tests/build/php/app.json.in) and
[tests/php/consumer.json.in](../tests/php/consumer.json.in); note that
[tests/php.sh](../tests/php.sh) retains `consumer.json.in` while commenting out
the legacy `php-apcu`/`php-xdebug` test harness).

- **[recipes/composer.json](../recipes/composer.json)** (`2.10.3`) runs on
  `"from": { "php": "..." }`.
- **[recipes/kanboard.json](../recipes/kanboard.json)** (`1.2.57`, `"from": {
  "php": "..." }`, `"sdk": { "composer": "...", "busybox": "..." }`) serves its
  app via [serve.php](../recipes/kanboard/serve.php) and
  [router.php](../recipes/kanboard/router.php) over service `"http"` on port
  `8471` (`"sandbox": "soft"`, with `"unsupported"` explaining that PHP's
  `is_writable()` reports false at low integrity), storing data in `${data}`.

### TeX Live

Upstream `tlnet` replaces package `.tar.xz` archives in place as packages
update, without versioned URLs. The committed recipe retains version
`2026.80721`: all 432 URL inputs are available in the source artifact next to
the digest-pinned legacy image listed in `source_mirrors`. This preserves the
original inputs when a live mirror changes or an archive server returns
bot-protection HTML. Downloaded bytes must still match each original SHA-256.
[scripts/texlive-pins.sh](../scripts/texlive-pins.sh)
generates [recipes/texlive.json](../recipes/texlive.json) from
[recipes/texlive/texlive.json.in](../recipes/texlive/texlive.json.in):

1. Downloads `texlive.tlpdb` from `MIRROR` (defaulting to
   `https://texlive.info/tlnet-archive/YYYY/MM/DD/tlnet`, using
   `TEXLIVE_SNAPSHOT` or yesterday's UTC date),
   resolves Windows package dependencies for `SCHEME` (default `scheme-small`,
   including `tlperl.windows`, `tlgs.windows`, and required packages), and
   verifies each `.tar.xz` archive's SHA-512 against `texlive.tlpdb`.
2. Writes [recipes/texlive/texlive.profile](../recipes/texlive/texlive.profile)
   configured for a portable tree at `TEXDIR B:/prefix` (`instopt_portable 1`),
   without documentation/source files and with `tlpdbopt_create_formats 0` (so
   timestamped formats are omitted from the image and created in container data
   on first use).
3. Expands `@SOURCES@` (the selected package archives plus `install-tl.zip` and
   `texlive.tlpdb`), `@VERSION@` (`<release>.<revision>`), and
   the snapshot date's UTC epoch `@EPOCH@` into `recipes/texlive.json`, pinning every
   archive URL and `sha256`.

For a future refresh, set `TEXLIVE_SNAPSHOT=YYYY/MM/DD` and run the script.
The default archive server can block unattended downloads; `MIRROR` can select
another accessible fixed snapshot. Set `TEXLIVE_PINS_CACHE` to retain verified
downloads between refreshes. Before replacing the committed pins, verify that
every input is available to CI and preserve it with `push --sources`; update
`source_mirrors` only to an image whose source artifact contains those inputs.
The old source artifact cannot supply packages introduced in a newer snapshot.

`texlive` uses `"layer": "estargz"` and `"from": { "perl": "...", "gs":
"ghostscript:..." }`, stripping TeX Live's own `tlperl` and `tlgs` in favor of
[recipes/perl.json](../recipes/perl.json) (Strawberry Perl 5.42.3.1) and
[recipes/ghostscript.json](../recipes/ghostscript.json) (10.08.0, built with
`zig`, `"entrypoint": "bin/gswin64c.exe"`). It points `TEXMFVAR`,
`TEXMFCONFIG`, `TEXMFHOME`, and `XE_FC_CACHEDIR` into `${data}`, patches
`fonts.conf` relative paths, `TLWinGoo.pm`, and `TLUtils.pm`, strips build
timestamps from generated configs, and refreshes `ls-R` via `mktexlsr.exe`.
The default entrypoint is `bin/windows/latexmk.exe`; invoke engines such as
`pdflatex`, `xelatex`, `lualatex`, `bibtex`, or `kpsewhich` with
`--entrypoint bin/windows/<tool>.exe texlive` (see
[tests/texlive.sh](../tests/texlive.sh)). Note that `kpathsea` cannot resolve
its own long path under AppContainers on Windows builds with `self-long-name`
(see [gaps.md](gaps.md)).

### PostgreSQL

- **[recipes/postgresql-sdk.json](../recipes/postgresql-sdk.json)** (18.6) builds
  PostgreSQL with `llvm-mingw` (`clang`), linking zlib, OpenSSL, lz4, zstd,
  libxml2, Windows' system `icu.dll` (via [icu-uc.pc](../recipes/postgresql/icu-uc.pc)
  and [icu-i18n.pc](../recipes/postgresql/icu-i18n.pc)), PL/Perl, and PL/Python.
  Its patches work around MinGW/Clang and AppContainer constraints:
  - [clang-mingw.patch](../recipes/postgresql/clang-mingw.patch): switches to
    MinGW's `setjmp`/`longjmp` and disables `__int128` (Clang's 16-byte struct
    alignment generated SSE `movaps` on 8-byte `palloc` buffers).
  - [signal-pipes.patch](../recipes/postgresql/signal-pipes.patch): moves signal
    pipes under `\\.\pipe\LOCAL\`.
  - [initdb-nul.patch](../recipes/postgresql/initdb-nul.patch) and
    [plperl-nul.patch](../recipes/postgresql/plperl-nul.patch): avoid redirecting
    or booting through `nul`, which AppContainers cannot open on some Windows
    releases.
  - [pg-config-long.patch](../recipes/postgresql/pg-config-long.patch): expands
    8.3 short paths so `pg_config` returns deterministic long paths.
  It also compiles [recipes/postgresql/pg-server.c](../recipes/postgresql/pg-server.c),
  renames `*.dll.a` to `*.a`, and strips build paths from `Makefile.global` and
  `.pc` files.
- **[recipes/postgresql.json](../recipes/postgresql.json)** (`18.6`, `"from": {
  "perl": "...", "python": "..." }`, `"sdk": { "pgsdk": ... }`) exposes
  `"services": [{ "name": "postgresql", "protocol": "tcp", "port": 5432 }]` and
  uses `pg-server.exe` as its entrypoint. On first run, `pg-server` runs
  `initdb` (`UTF8`, `builtin` provider `C.UTF-8`), generates a 24-character
  password stored in `${data}\pgpass.conf`, and starts `postgres` in the
  foreground. Client runs (`psql`, `pg_dump`, `pg_ctl`) connect using
  `--entrypoint=<tool>` and `--port=postgresql=<port>` (see
  [tests/postgresql.sh](../tests/postgresql.sh)).

The three legacy extension recipes (`postgresql-pg-cron`, `postgresql-pgvector`,
`postgresql-postgis`) were removed in commit `df24cf4`. However, `postgresql-sdk`
still ships `pg_config` and `pgxs` (`lib/postgresql/pgxs`) for building C
extensions, and `pg-server.c` still reads `PG_EXTENSION_DIRS` and
`PG_PRELOAD_LIBRARIES` from the environment if a custom image composes
extensions in `"from"`.

### GDAL

[recipes/gdal-sdk.json](../recipes/gdal-sdk.json) (3.13.3) builds GEOS 3.15.0,
PROJ 9.9.0, SQLite 3530400 (with RTREE for GeoPackage), `json-c` 0.19, `expat`
2.10.0, `libiconv` 1.19, and GDAL (with internal TIFF, PNG, and JPEG) using
`llvm-mingw`, `cmake`, `meson`, and `busybox`, stripping `B:/prefix` paths from
`gdal-config` and `.pc` files.

[recipes/gdal.json](../recipes/gdal.json) keeps the runtime binaries, `PROJ_DATA`,
and `GDAL_DATA` with `"entrypoint": "bin/gdal.exe"` and `"permissions": {
"filesystem": ["cwd"] }`. Other utilities (`gdalinfo`, `gdalwarp`, `ogr2ogr`,
etc.) run via `--entrypoint` (see [tests/gdal.sh](../tests/gdal.sh)).

### osm2pgsql

[recipes/osm2pgsql.json](../recipes/osm2pgsql.json) (2.3.1) builds `bzip2`, Lua
5.4.9, `nlohmann/json`, header-only Boost 1.92.0 (with
[BoostConfig.cmake](../recipes/osm2pgsql/BoostConfig.cmake)), and `osm2pgsql`
against `postgresql-sdk` (`libpq`) and `gdal-sdk` (`PROJ`). It applies
[default-style.patch](../recipes/osm2pgsql/default-style.patch) so the relative
`default.style` path resolves next to the executable instead of `cwd`.

To connect to a running `postgresql` container across AppContainer isolation, the
recipe declares `"uses": [{ "name": "postgresql", "port": 5432 }]` and `"env": {
"PGPORT": "${uses:postgresql}" }` (verified in
[tests/osm2pgsql.sh](../tests/osm2pgsql.sh)).

### Chromium and Electron

Chromium and Electron ship a multi-process sandbox that Windows rejects inside
low-integrity tokens and AppContainers (see [findings.md](findings.md) and
[tests/chromium.sh](../tests/chromium.sh)), so both images require `"sandbox":
"soft"`:

- **[recipes/vscodium.json](../recipes/vscodium.json)** (1.143.17232, `"layer":
  "estargz"`, `"from": { "git": "mingit:..." }`) grants `"permissions": {
  "sandbox": "soft", "network": true, "host": true, "filesystem": ["cwd"] }`,
  strips `updateUrl` and `setAsDefaultProtocolClient` from `product.json` so
  VSCodium does not register URL schemes on the host, removes
  `/bin/codium-tunnel.exe`, and keeps user/extension state in `${data}`.
- **[recipes/no-publish/ungoogled-chromium.json](../recipes/no-publish/ungoogled-chromium.json)**
  (156.0.8078.73-1.1) grants `"sandbox": "soft"`, notes in `"unsupported"` why
  `low` and `appcontainer` fail, keeps profile data in the container's
  `${data}`, and isolates `"registry": ["Software\\Chromium"]` in a container
  registry hive.

### MSVC

Recipes that require Microsoft Visual C++ or the Windows SDK declare `"host":
["msvc"]` (currently the only accepted `"host"` value) and must contain at least
one stage with `"build"` commands. During the build, Quasidock locates Visual
Studio with `vswhere.exe` (`-requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64`),
imports the environment from `vcvars64.bat`, exposes it to build stages, and
records the host's `"msvc"` (`VCToolsVersion`) and `"windows-sdk"` versions
under `build.host` in the image config.

Because host-toolchain outputs vary by Visual Studio installation:

- `quasidock build` skips build-cache reuse whenever `"host"` is non-empty.
- `quasidock` prints a warning at install (`warning: built with the host's msvc,
  so this image won't rebuild the same elsewhere`).
- [tests/published.sh](../tests/published.sh) does not compare host-built images
  for bit-for-bit reproducibility.

[recipes/gtk4-msvc.json](../recipes/gtk4-msvc.json) (`4.24.1`, see also
[tests/build/msvc/hello.json](../tests/build/msvc/hello.json)) demonstrates an
MSVC build (`CL="/Brepro ..."`, `LINK="/Brepro ..."`) of GLib, GTK 4, and
`libadwaita` (`proxy-libintl`, without `librsvg`, `libsoup`, spellchecking, or
translations), bundling `$VCTOOLSREDISTDIR` CRT DLLs into `bin/` so all imports
bind inside the image ([tests/gtk.sh](../tests/gtk.sh)).

## Legacy Zigsaw differences

Quasidock breaks compatibility with legacy Zigsaw recipes, images, and CLI
features:

- Reverse-DNS IDs (`org.nodejs.node`, `net.frippery.busybox`, etc.) were replaced
  with short Docker-style IDs (`node`, `busybox`, etc.).
- `entrypoint` (required) and `cmd` replace `command`, `args`, `exports`, and
  run-time `--command`. Image configs without `entrypoint` fail validation; run
  other binaries in an image with `quasidock run --entrypoint=... IMAGE [ARGS...]`.
- `from` (single-level alias map) replaces `runtimes`. Base images that declare
  their own `from` are rejected, and all runtime IDs in `from` must be unique.
- `stages` replaces `modules` (see [iteration 2](iteration-2.md) through
  [iteration 4](iteration-4.md)), and remote image references in recipes require
  `@sha256:...` digest pins.
- **Extension points and extension recipes removed**: `extensions` slots,
  automatic extension mounting, `--noextension`, and the 12 extension recipes
  (`gtk4-hunspell-*`, `php-*`, `postgresql-*`) were removed in commit `df24cf4`.
  Compose multi-image stacks explicitly with `from`.
- **Desktop integration, overrides, and user-folder permissions removed**:
  Start menu `shortcuts`, file `associations`, URL `schemes`, host `bin\` shims,
  `--file` path translation, user-folder junctions (`desktop`, `documents`,
  `downloads`, etc.), `--ephemeral` (replaced by `--rm`), and persistent recipe
  `override` layers were removed. Filesystem access is granted via
  `permissions.filesystem` (`"cwd"` / `"cwd:ro"`) or `--filesystem=cwd|<abs>`.
