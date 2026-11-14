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
| `name` | No | Display name shown in metadata. |
| `summary` | No | Short one-line summary. |
| `homepage` | No | `https://` URL (at most 256 bytes). |
| `license` | No | SPDX license identifier or expression. |
| `path` | No | String array of directories added to `PATH` when this image runs or provides `from`/`sdk` tools. Defaults to `["."]`. |
| `env` | No | Map of environment variables set for the image and downstream builds; values support placeholders. |
| `permissions` | No | Sandbox, network, filesystem, and service permissions object. |
| `registry` | No | Array of `HKCU\Software\...` subkeys persisted per container (for example `"Software\\JavaSoft\\Prefs\\net\\sourceforge\\plantuml"`). Excludes `Software` itself, `Classes`, `Microsoft`, `Policies`, `Wow6432Node`, `AppDataLow`, and `quasidock`, and keys must not overlap. |
| `from` | No | Map `{ "<alias>": "<image-ref>@sha256:..." }` of pinned base images shipped as lower OCI layers with the app. Single-level only: `from` images cannot declare `from` images of their own. |
| `sdk` | No | Map `{ "<alias>": "<image-ref>@sha256:..." }` of pinned build-time tool images mounted into the build environment (not shipped in the built image). |
| `host` | No | Host toolchains required during build. Currently only `["msvc"]` is accepted, and requires at least one stage with `build` commands. |
| `layer` | No | App layer format: `"gzip"` (default) or `"estargz"`. |
| `prefetch` | No | `/`-separated relative paths (case-insensitively unique) prefetched on lazy pull. Requires `"layer": "estargz"`. |
| `cleanup` | No | Files or paths removed after stages complete before packaging the app layer. |

Alias names in `from` and `sdk` must match `[a-z][a-z0-9_-]{0,31}`, cannot be
`app`, `data`, `cache`, or `roots`, and cannot overlap between `from` and `sdk`.

### Placeholders

Placeholders expand at build time (for `from`/`sdk` tool paths/env) and at
container runtime (for `entrypoint`, `cmd`, `path`, and `env`):

- `${app}`: the image's own root directory.
- `${data}`: the container's isolated data directory (`containers/<id>/data`).
- `${cache}`: the cache directory.
- `${roots}`: the CA root certificate bundle path.
- `${<alias>}`: the root directory of the `from` base image named `<alias>`.
- `${listen:<service>}`, `${port:<service>}`: listen address and port for a declared service.
- `${uses:<name>}`: endpoint/port for a declared service dependency in `permissions.uses`.

At runtime, `run -v 'NAME:${data}'` attaches a named volume to the complete data
tree. A target such as `${data}/workspace` preserves only that subtree; bind
sources use the same targets. Mounts are run options rather than recipe fields.
Mounted contents survive `--rm`, including registry state when its backing
directory is mounted. See [persistent storage](../README.md#persistent-storage)
for source syntax, sharing, and explicit volume cleanup.

## `permissions`

- `sandbox`: `"soft"`, `"low"` (default), or `"appcontainer"`.
- `network`: boolean enabling network access in sandboxed runs.
- `filesystem`: array of `"cwd"` or absolute Windows paths, optionally suffixed with `:ro` or `:rw`.
- `services`: array of `{ "name": "...", "port": <n>, "command": ... }` service declarations (`command` is optional).
- `uses`: array of `{ "name": "...", "port": <n> }` service dependencies.
- `unsupported`: map keyed by `"soft"`, `"low"`, or `"appcontainer"` giving a reason (at most 256 bytes) why that sandbox mode cannot be used; cannot match the image's chosen `sandbox`.
- `host`: optional boolean.

## `stages`, `sources`, and `vendor`

Stages run in order during `quasidock build`. Everything installed under
`B:\prefix` (`PREFIX=B:\prefix`) becomes the app's OCI layer. After all stages
complete, `quasidock build` checks that `entrypoint[0]` resolves to a file in
the built app tree or in the referenced `from` image.

Each stage object supports:

- `name`: `1`–`57` characters matching `[A-Za-z0-9._-]`, case-insensitively unique across the recipe.
- `sources`: array of source objects. A stage must specify `sources` or `build`.
- `build`: array of commands to run in `B:\src\<stage>`. When `build` is omitted, `sources` unpack directly into `B:\prefix`; when `build` is present, `sources` unpack into `B:\src\<stage>`.
- `shell`: `"sh"` (default, using `sh.exe` or `busybox.exe` on tool `PATH`) or `"cmd"` (`cmd.exe /d /s /c`). If `build` uses `sh` and no image in `sdk` or `from` provides it, add `busybox` to `sdk`.
- `env`: map of environment variables for the stage (requires `build`).
- `network`: boolean (default `false`; requires `build`). When `false`, `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` are set to `http://127.0.0.1:9` and `NO_PROXY` is cleared during `build`.
- `vendor`: `{ "commands": [...], "dir": "...", "sha256": "..." }` (requires `build`). `vendor.commands` run with network access enabled inside `B:\src\<stage>`; `vendor.dir` (a relative path inside the stage directory) is archived and cached under `downloads/<sha256>`. Omit `sha256` on the first run to have `quasidock build` print the digest to pin, then set the 64-character lowercase hex digest so subsequent builds stay offline and reproducible.

Each entry in `sources` must set **exactly one** of `url`, `path`, or `image`:

- `url`: HTTP/HTTPS download. Non-directory URLs require `sha256`. Optional `type` (`file`, `zip`, `tar`, `tar.gz`, `tar.xz`; `.tgz` maps to `tar.gz`, `.txz` maps to `tar.xz`, `.crate` maps to `tar.gz`), `dest` (relative target path), and `strip` (archive path components to strip; archives only).
- `path`: file or directory relative to the recipe directory (`"."` allowed). Directory `path` sources support `skip` patterns (`"/<path>"` anchored patterns or filename globs without `/` or `\`).
- `image`: image reference (`@sha256:...` pinned); `sha256`, `type`, `strip`, and `skip` are not allowed.

### Build environment

During `build` and `vendor` commands, Quasidock sets:

- `PREFIX=B:\prefix`
- `SOURCE_DATE_EPOCH=315532800`
- `QUASIDOCK_BUILD=1`
- `ZIG_LOCAL_CACHE_DIR=B:\src\<stage>\.zig-cache`
- `ZIG_PROGRESS=off`
- `PKG_CONFIG_PATH` and `CMAKE_PREFIX_PATH` pointing to `B:\prefix` and tool images (`from` / `sdk`)
- MSVC / Windows SDK environment variables and `PATH` entries when `host` includes `"msvc"`

## Pins and rebuilding

All `from`, `sdk`, and `source.image` references must be pinned with
`@sha256:<manifest-digest>`. If an unpinned reference is passed, `quasidock build`
fails and prints the resolved `windows/amd64` manifest digest to pin.

- [scripts/published-recipes.txt](../scripts/published-recipes.txt) lists standalone published recipes.
- [scripts/published-groups.txt](../scripts/published-groups.txt) lists grouped recipe build sets.
- [scripts/rebuild.sh](../scripts/rebuild.sh) rebuilds all recipes cleanly in dependency order and checks reproducibility.

## TeX Live

Upstream `tlnet` replaces package `.tar.xz` archives in place as packages
update, without versioned URLs. [scripts/texlive-pins.sh](../scripts/texlive-pins.sh)
generates [recipes/texlive.json](../recipes/texlive.json) from
[recipes/texlive/texlive.json.in](../recipes/texlive/texlive.json.in):

1. Downloads `texlive.tlpdb` from `MIRROR` (defaulting to MIT's CTAN mirror),
   resolves Windows package dependencies for `SCHEME` (default `scheme-small`,
   including `tlperl.windows`, `tlgs.windows`, and required packages), and
   verifies each `.tar.xz` archive's SHA-512 against `texlive.tlpdb`.
2. Writes [recipes/texlive/texlive.profile](../recipes/texlive/texlive.profile)
   configured for a portable tree at `TEXDIR B:/prefix` (`instopt_portable 1`),
   without documentation/source files and with `tlpdbopt_create_formats 0` (so
   timestamped formats are omitted from the image and created in container data
   on first use).
3. Expands `@SOURCES@`, `@VERSION@` (`<release>.<revision>`), and a daily UTC
   epoch `@EPOCH@` into `recipes/texlive.json`, pinning every archive URL and
   `sha256`.

## Gradle

[recipes/gradle.json](../recipes/gradle.json) builds the `gradle` image on
`"java"` (`from`), invoking Java with `-Dorg.gradle.daemon=false` in its
`entrypoint` and packaging two helper files for reproducible offline builds:

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

## Legacy Zigsaw differences

The current schema breaks compatibility with legacy Zigsaw recipes and images:

- Reverse-DNS IDs (`org.nodejs.node`, `net.frippery.busybox`, etc.) were replaced
  with short Docker-style IDs (`node`, `busybox`, etc.).
- `entrypoint` (required) and `cmd` replace `command`, `args`, and `exports`.
  Image configs without `entrypoint` fail validation.
- `from` (single-level alias map) replaces `runtimes`.
