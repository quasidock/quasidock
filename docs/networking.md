# Networking

In Quasidock, [`tests/cmd/netprobe/main.go`](../tests/cmd/netprobe/main.go) builds
`quasidock-netprobe.exe` (`serve`, `ping`, `env`, `wait`, and a canned summary
`report`), while [`tests/network.sh`](../tests/network.sh) executes the live
integration checks for proxies, the AppContainer profile race, services,
loopback exemptions, and forwards.

## Baseline behavior

- Images declare `permissions.network` (default `false`); `quasidock run`
  overrides it per run with `--share=network` or `--unshare=network` (see
  [`internal/cli/cli.go`](../internal/cli/cli.go).
- Under `--sandbox=appcontainer`, a run with `network` holds the
  `internetClient` (`S-1-15-3-1`) and `privateNetworkClientServer`
  (`S-1-15-3-3`) capabilities alongside its own package capability (`quasidock.<id>`).
  Without `network`, it holds only its package capability. `appcontainer` is
  the only sandbox that enforces `network`.
- Under `soft` and `low`, `network` is not enforced: an app can reach whatever
  your account can.
- During `quasidock build`, offline `build` stages (`build.network` false) set
  `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` to `http://127.0.0.1:9` and clear
  `NO_PROXY`, while `vendor` stages run with network access
  ([`internal/builder/builder.go`](../internal/builder/builder.go)).
- Quasidock's own HTTP clients honor proxy environment variables via
  `http.ProxyFromEnvironment` in
  [`internal/registry/registry.go`](../internal/registry/registry.go) and
  `http.DefaultTransport` in
  [`internal/builder/fetch.go`](../internal/builder/fetch.go). Because
  [`internal/environment/environment.go`](../internal/environment/environment.go)
  builds each container's environment from scratch, proxy variables must be
  explicitly forwarded into runs with `network`.
- AppContainer loopback isolation blocks cross-container and host-container
  connections by default, which is why:
  - a host browser must reach [`kanboard`](../recipes/kanboard.json) in its
    container (closed by services);
  - external clients must reach [`postgresql`](../recipes/postgresql.json) in
    its container (closed by services);
  - [`osm2pgsql`](../recipes/osm2pgsql.json) must reach a PostgreSQL server on
    this machine (closed by loopback exemption and `uses` forwards); and
  - a custom `from` base image such as an Xdebug-enabled PHP layer must reach an
    IDE debugger on the host (closed by declaring `uses`; extensions themselves
    and `php-xdebug` were removed in Quasidock [iteration 3](iteration-3.md)).

## What Windows does

Measured unelevated on Windows 11 26300 and Server 2025 CI with
separate endpoint processes, passing only on round-trip echo:

| path | result |
| --- | --- |
| between runs in one AppContainer (one package SID) | works, with no network capability at all, and with different capabilities on each side |
| from one AppContainer to another | dropped (the connection times out) |
| from the host to an AppContainer, or from an AppContainer to the host | dropped, also through the machine's LAN address, and with every network capability |
| a listening socket the host made, handed to an AppContainer process | host clients connect, and the container accepts them |
| a socket the host connected, handed to an AppContainer process | the container talks to the host's server |
| `WSADuplicateSocket` into an AppContainer process that's already running | works |
| an unconnected socket the host made, handed in | the container connects it to the host, and to the internet with no network capability |
| `AF_UNIX`, between the host and a container either way, or two containers | works, with no network capability. Only the socket file's ACL decides, which it inherits from its directory: a client that may only read and execute it is refused (`10013`), as is a container not granted it at all |
| `AF_UNIX`, from a low-integrity process to a socket a medium-integrity one made | refused (`10013`): connecting writes to the socket file, which has its directory's label (medium, by default). The host reaches a low-integrity server's socket, in a directory labelled low |
| `AF_UNIX` paths | up to 108 bytes, all of `sun_path`, with no room for a NUL. PostgreSQL refuses a path of 108 or more |
| a named pipe the host makes for one capability | opens only from a process that holds that capability |
| `OpenProcess` for memory writes and handle duplication | allowed within one AppContainer, even with different capabilities. Denied across containers |
| `CheckNetIsolation LoopbackExempt -a -p=<sid>`, unelevated (Administrators member with filtered UAC token) | succeeded. The exempt container then reached the host, also through the LAN address. Connections into containers were still dropped |
| `NetworkIsolationSetAppContainerConfig`, the API underneath, unelevated | succeeds in 2–4 ms (2–7 ms for a standard user on Windows 11 26300); taking the exemption back also succeeds |
| an exempt container with no network capability, to the host | connects: the exemption doesn't need `internetClient` or `privateNetworkClientServer` |
| the host to an exempt container | dropped |
| `CreateAppContainerProfile` of one new name, from 8 threads at once | some calls fail with `0x8007000a` or `0x8000ffff` (or `0x80070003` on CI) instead of `E_ALREADY_EXISTS` |

Not tested in those probes: two exempt containers talking to each other, or
inbound exemptions (`-is`).

Implications for Quasidock:

- **A container's runs already share a private loopback.** Same-container
  clients reach same-container servers without special privileges.
- **A socket keeps the rights of the process that made it.** Handing an
  unconnected host socket into a container would bypass `network`.
- **Two apps in one container are one app.** Processes inside the same
  AppContainer can `OpenProcess` each other, so sharing one container is not an
  isolation boundary.
- **Concurrent first runs need profile serialization.**
  [`internal/appcontainer/appcontainer.go`](../internal/appcontainer/appcontainer.go)
  guards `CreateAppContainerProfile` with named mutex
  `Local\quasidock-appcontainer`, acquiring and releasing it on the same OS
  thread, and retries once after `100 ms` if it returns a
  non-`E_ALREADY_EXISTS` error (exercised by the 8-way concurrent startup test
  in [`tests/network.sh`](../tests/network.sh)).

## Principles

1. **One meaning in every sandbox, enforced by AppContainer.**
   - Without `network`, an app has only its own private loopback: its runs reach
     each other (as `psql` reaches `pg-server.exe`).
   - With `network`, it reaches the internet, the LAN, and local host servers.
   - `soft` and `low` cannot enforce this boundary without elevated Windows
     filtering.
2. **Serving is declared.** An app reachable from outside declares services
   (`permissions.services`); clients use the same port placeholders in every
   sandbox.
3. **Apps never share a container.**
4. **Only connected or bound sockets enter a container.** `quasidock.exe`
   accepts or connects outside the container and passes sockets or handles to
   `quasidock-relay.exe`; an app can inspect any process inside its own
   container.
5. **Nothing requires elevation.** Elevated actions (such as adding a test CA to
   `LocalMachine\Root` in [`tests/certs.sh`](../tests/certs.sh)) stay an
   explicit user step.

## Design

### Private loopback

Runs of the same app inside an AppContainer already reach each other on
`127.0.0.1` with no `network` capability. [`tests/network.sh`](../tests/network.sh)
verifies this by starting `quasidock-netprobe.exe serve` without `network` and
reaching it with `exec` using the server container's `${listen:<name>}`.

### Services: reaching an app in a container

Declared in a recipe's `permissions.services` array (`{ "name": "...", "port": <n>, "command": "..." }`,
where `command` is optional):

```json
"permissions": {
  "network": true,
  "services": [
    { "name": "postgresql", "port": 5432, "command": "bin/pg-server.exe" }
  ]
}
```

- [`recipes/postgresql.json`](../recipes/postgresql.json) declares `postgresql`
  on port `5432` with `"command": "bin/pg-server.exe"` (matching its
  `"entrypoint": "bin/pg-server.exe"`), so auxiliary utilities like `psql` or
  `pg_isready` do not bind port `5432`.
- [`recipes/kanboard.json`](../recipes/kanboard.json) declares
  `{ "name": "http", "port": 8471 }` without a `command` field; because
  `command` is omitted, `services.Serves` in
  [`internal/services/services.go`](../internal/services/services.go) matches
  its default `entrypoint` (`["${php}\\php.exe", "${app}\\serve.php"]`).
- When an explicit `COMMAND` or `--entrypoint` is supplied to `quasidock run`,
  `services.Serves` matches non-empty `s.Command` against that command, or
  if `s.Command` is empty, requires a single-element `entrypoint` matching it.

Each declared service provides two placeholders
([`internal/oci/oci.go`](../internal/oci/oci.go),
[`internal/services/services.go`](../internal/services/services.go)):

- `${listen:<name>}`: the port the app listens on (and where same-container runs
  connect, such as `PGPORT` in PostgreSQL or `KANBOARD_LISTEN` in Kanboard).
- `${port:<name>}`: the published host-facing port where external clients reach
  the service (such as `KANBOARD_URL` = `http://127.0.0.1:${port:http}/`).

Behavior by sandbox:

- **Under `soft` and `low`**: both `${listen:<name>}` and `${port:<name>}`
  equal the declared port (or the port given by `--port`). The app binds it
  directly.
- **Under `appcontainer`**: `services.Resolve` reserves an internal port
  per container and service from `20000`–`48999` (`internalFirst = 20000`,
  `internalCount = 29000`, below Windows' dynamic port range, verified free on
  `127.0.0.1` and `::1`), persisted in
  `<store>\containers\<container-id>\ports.txt` outside mounted data.
  A store-wide allocator lock prevents simultaneous starts from reserving the
  same port. Restarts reuse free ports and replace occupied ones; removing or
  pruning a container also removes its reservations. Legacy image-scoped port
  files remain reserved while older supervisors may be running. `exec` uses the
  running container's environment; separate clients need an explicit endpoint.
  `${listen:<name>}` expands to that internal port while `${port:<name>}` expands
  to the published port.
  - Before starting a matching serving command, `services.Publish` binds the
    published port on `127.0.0.1` and `::1` (`SO_EXCLUSIVEADDRUSE` in
    [`internal/net/net.go`](../internal/net/net.go)) outside the container.
  - `quasidock.exe` copies `quasidock-relay.exe` (next to `quasidock.exe`) to
    `<store>\relay\quasidock-relay-<8hex>.exe`, grants the AppContainer
    read+execute access on the `relay` directory, and spawns it in the run's
    Windows job (`DETACHED_PROCESS`) with `CapabilityCount = 1` (the app's own
    capability SID only, with no network capabilities), inheriting the bound
    socket handles as `<socket>=<internal-port>` arguments.
  - [`cmd/quasidock-relay/main.go`](../cmd/quasidock-relay/main.go) accepts
    connections on those listening sockets, connects to the internal port on
    `127.0.0.1` or `::1` (retrying up to `20` times with `100 ms` sleeps while
    the app starts listening), and pumps `16 KiB` buffers bidirectionally.
  - `quasidock.exe` holds its own copies of the listening sockets until the run
    exits (`published.Close()`), releasing the port when the container finishes.

Operational rules:

- Publishing is automatic under `appcontainer` whenever the matching serving
  command runs; nothing extra needs to be passed.
- `--port=<service>=<port>` moves the published port for that run. Saved
  overrides were removed in Quasidock [iteration 3](iteration-3.md), and
  Docker-style `-p` is not supported ([`internal/cli/cli.go`](../internal/cli/cli.go)).
  `quasidock image info` and install output list `serves`.
- If the published port is already bound (`WSAEADDRINUSE` / `WSAEACCES`),
  `services.Publish` prints a note (`note: port <port> is taken, so <service> isn't published this run`)
  and lets the run continue unpublished.
- Services listen on loopback (`127.0.0.1` and `::1`) only.
- In [`recipes/postgresql.json`](../recipes/postgresql.json), `PGPORT` is set to
  `${listen:postgresql}` (`postgres.exe` and `libpq` clients honor it); passing
  PostgreSQL's own `-p` flag to `postgres.exe` or `psql` overrides `PGPORT` and
  bypasses the service's internal port mapping.

### Reaching this machine from a container

Quasidock uses two mechanisms depending on `network` and Windows policy.

#### (a) Loopback exemption

In [`internal/run/run.go`](../internal/run/run.go), an AppContainer run with
`network` enabled checks `QUASIDOCK_LOOPBACK_EXEMPTION` and, unless it is `"0"`,
calls `exemptLoopback`:

- It derives the app's package SID (`quasidock.<id>`) and serializes access to
  Windows' exemption table under mutex `Local\quasidock-loopback`
  ([`internal/appcontainer/appcontainer.go`](../internal/appcontainer/appcontainer.go)).
- If the SID is not already exempt (`appcontainer.IsExempt`) and not recorded
  in `<store>\grants\<id>.txt`, it records `loopback` (`store.GrantLoopback`)
  and adds the SID via `NetworkIsolationSetAppContainerConfig`
  (`appcontainer.Exempt`). Manually pre-existing exemptions are left unrecorded.
- When exempt, connections to `localhost:<port>` work transparently just like
  under `soft`, across all host loopback listeners.
- `QUASIDOCK_LOOPBACK_EXEMPTION=0` skips `exemptLoopback`, keeping the
  container loopback-isolated without modifying the machine-wide firewall list.
- If Windows refuses the exemption call, `exemptLoopback` removes any newly
  added `loopback` grant record, prints a `note:` message explaining the
  fallback, and proceeds with `LoopbackIsolated`.

#### (b) Forwards

Apps and `from` base images can declare host/service ports they connect to in
`permissions.uses` (`{ "name": "...", "port": <n> }`):

- [`internal/services/services.go`](../internal/services/services.go) (`UsedBy`)
  merges `cfg.Permissions.Uses` and every `r.Uses` from `cfg.From` base images,
  deduplicated case-insensitively by name (note extensions and `--extension`
  were removed in Quasidock [iteration 3](iteration-3.md), so custom add-on
  layers participate via `from`).
- [`recipes/osm2pgsql.json`](../recipes/osm2pgsql.json) declares
  `"uses": [{ "name": "postgresql", "port": 5432 }]` and sets
  `"PGPORT": "${uses:postgresql}"`.
- `--port=<name>=<port>` moves a `uses` target port as well (`services.Targets`),
  and `quasidock image info` / install output list `uses`.

When an AppContainer run is `LoopbackIsolated` (because `network` is off,
`QUASIDOCK_LOOPBACK_EXEMPTION=0` is set, or Windows refused exemption) and has
at least one `uses` entry:

1. `services.ForwardThrough` selects a local port per target from
   `20000`–`48999` and expands `${uses:<name>}` to that port.
2. `forward.Open` ([`internal/forward/forward.go`](../internal/forward/forward.go))
   creates a pipe pair inherited by `quasidock-relay.exe` (`forward=<in>:<out>`
   and `use=<port>` arguments).
3. `quasidock-relay.exe` listens on `127.0.0.1` and `::1` for each forwarded
   port and sends a ready byte over the pipe so `quasidock.exe` waits until the
   listeners are up before starting the app.
4. For each incoming connection from the container, the relay sends the target
   index to `quasidock.exe`, which connects outside the container (`127.0.0.1`
   then `::1`, `2000 ms` timeout), calls `WSADuplicateSocketW` targeting the
   relay PID, sends `WSAPROTOCOL_INFOW` back, waits for the relay's ack byte,
   and closes its copy while the relay pumps bytes both ways.

Forwards change no system-wide settings, permit only declared `uses` ports even
without `network`, and require the app to read `${uses:<name>}` from its
configuration/environment. `--verbose` prints whether a run used
`loopback exempt` or `loopback isolated` (with or without forwards) and each
mapped `uses` port.

### AF_UNIX: an unimplemented alternative

Quasidock does not currently implement an `AF_UNIX` socket placeholder, because
services, loopback exemption, and `uses` forwards cover existing apps. If added
later (e.g. `unix_socket_directories` + `PGHOST` for PostgreSQL), the previous
findings constrain the design:

- **PostgreSQL names sockets by port**: `<dir>\.s.PGSQL.<port>` uses the same
  port number PostgreSQL listens on over TCP. Under `appcontainer`, a server
  uses its internal port while external or forwarded clients see the published
  or relay port, so socket discovery cannot assume matching port numbers.
- **Clients need Modify access and matching integrity**: connecting to an
  `AF_UNIX` socket writes to the socket file (`10013` with Read+Execute only),
  so clients need Modify on the socket directory; under `--sandbox=low`, the
  directory also needs a low integrity label for low-integrity clients or
  servers to connect.
- **Paths are capped at 108 bytes**: `sun_path` has no room for a NUL beyond
  108 bytes, and PostgreSQL refuses paths of 108 bytes or longer. While
  Quasidock uses short image IDs (`[A-Za-z0-9._-]`, up to 57 characters) rather
  than reverse-DNS IDs, a user-chosen `QUASIDOCK_HOME` path can still push a
  per-app directory over the limit unless hashed or named by service.

### Proxies

- **Runtime runs**: when `network` is enabled, `environment.Build`
  ([`internal/environment/environment.go`](../internal/environment/environment.go),
  called with `Proxies: network` in [`internal/run/run.go`](../internal/run/run.go))
  copies `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and `NO_PROXY` from the host
  environment using case-insensitive lookup. Image `env` variables (including
  `from` base images) and CLI `--env`/`-e` overwrite earlier values
  case-insensitively, so app config and `--env` win.
- **Offline runs under `soft` and `low`**: when `network` is disabled, no proxy
  variables are copied and no dead proxy is injected, so apps that default to
  `low`/`soft` without declaring `network` (for instance [`meld`](../recipes/meld.json)
  invoking its bundled `mingit`) keep working unchanged.
- **Builds**: during `quasidock build`, offline `build` stages (`build.network`
  false) receive `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` set to
  `http://127.0.0.1:9` and `NO_PROXY=""`, while `vendor` stages run without the
  dead proxy ([`internal/builder/builder.go`](../internal/builder/builder.go)).

### Certificate trust

Enterprise or test HTTPS proxies install their own root CA in Windows' machine
certificate store. Most Quasidock images read Windows' stores natively; the rest
read **`${roots}`**, a PEM file generated on demand by
[`internal/roots/roots.go`](../internal/roots/roots.go).

| image | TLS | trusts |
| --- | --- | --- |
| `quasidock` itself | Go (`crypto/tls`) | Windows' root store (plus `http.ProxyFromEnvironment` in [`internal/registry/registry.go`](../internal/registry/registry.go) and `http.DefaultTransport` in [`internal/builder/fetch.go`](../internal/builder/fetch.go)) |
| `zig` (`zig fetch`) | zig's std | Windows' roots. At `low` integrity, zig's std fails opening the store to write (`CertificateBundleLoadFailure`); its recipe omits `network` so `tests/certs.sh` passes `--share=network` |
| `gtk4` (`libsoup`), and apps on it | `glib-networking` with OpenSSL | Windows' roots and intermediates, patched in [`gtk4-sdk.json`](../recipes/gtk4-sdk.json) to open stores read-only (`CERT_STORE_READONLY_FLAG`) for `low` integrity |
| `python`, `pygobject` | OpenSSL (`ssl` default context) | Windows' store (via the `ssl` module's default context) and `SSL_CERT_FILE` |
| `mingit`, `meld`'s git | Schannel | Windows' stores. [`mingit/gitconfig`](../recipes/mingit/gitconfig) avoids Git for Windows' system config and sets `http.sslBackend = schannel` |
| `node`, `prettier` | OpenSSL | Node's built-in roots plus Windows' store (`NODE_USE_SYSTEM_CA=1` in [`node.json`](../recipes/node.json)) |
| `java`, `java-sdk`, and JVM apps | JSSE | Machine roots (`-Djavax.net.ssl.trustStoreType=Windows-ROOT-LOCALMACHINE` in `JAVA_TOOL_OPTIONS` in [`java.json`](../recipes/java.json) and [`java-sdk.json`](../recipes/java-sdk.json)) instead of Temurin's `cacerts` |
| `go`, `zot` | Go (`crypto/tls`) | Windows' root store |
| `rust` (`cargo`), `cmake` | curl with Schannel | Windows' stores. Schannel/curl checks revocation strictly: a certificate without a reachable CRL or OCSP responder is rejected |
| `qt6`, `flameshot`, `notes` | Schannel | Windows' stores |
| `vlc` | GnuTLS | Windows' stores |
| `php` | OpenSSL | PHP streams read Windows' stores (`openssl.cafile` unset); PHP curl reads `${roots}` via `curl.cainfo = "${SSL_CERT_FILE}"` in [`php-sdk/php.ini`](../recipes/php-sdk/php.ini) (`SSL_CERT_FILE` set to `${roots}` in [`php.json`](../recipes/php.json)) |
| `composer` | PHP (`from.php`) | `${roots}` via inherited `SSL_CERT_FILE` before Composer's own bundle |
| `postgresql` (`psql`), `osm2pgsql` | OpenSSL (`libpq`) | `sslrootcert` (default `root.crt` in app data), or `${roots}` via `SSL_CERT_FILE=${roots}` when `sslrootcert=system` |
| `perl` | `IO::Socket::SSL` | `${roots}` via `SSL_CERT_FILE=${roots}` in [`perl.json`](../recipes/perl.json) before `Mozilla::CA` (its recipe omits `network`) |
| `busybox` (`wget`) | BusyBox's own | Does not verify TLS certificates (its recipe omits `network`) |

Key details:

- **Java uses `Windows-ROOT-LOCALMACHINE`**, not `Windows-ROOT`: reading the
  per-user store in `Windows-ROOT` opens it writable and fails at `low`
  integrity (`KeyStoreException: error 5`).
- **Windows does not fetch unknown roots on store enumeration**: unlike
  native Windows APIs that lazily fetch missing roots via CTL updates, Java,
  Python, and `${roots}` see only certificates already in Windows' stores.
- **How `${roots}` works** ([`internal/roots/roots.go`](../internal/roots/roots.go)):
  - Triggered at runtime when `cfg.Env`, `cfg.Entrypoint`, `cfg.Cmd`, or any
    `cfg.From[*].Env` references `${roots}` (`usesRoots` in
    [`internal/run/run.go`](../internal/run/run.go)), and during builds when a
    tool's `env` references `${roots}` (`internal/builder/builder.go`).
  - `roots.File` reads `CA` and `ROOT` stores read-only
    (`CERT_STORE_READONLY_FLAG`) across seven Windows store locations
    (`LOCAL_MACHINE`, `LOCAL_MACHINE_ENTERPRISE`,
    `LOCAL_MACHINE_GROUP_POLICY`, `CURRENT_USER`,
    `CURRENT_USER_GROUP_POLICY`, `SERVICES`, and `USERS`), keeps certificates
    permitting `serverAuth` (`1.3.6.1.5.5.7.3.1`), deduplicates and sorts them
    by SHA-256, and writes `<store>\certs\roots-<8hex>.pem` (named by the first
    4 bytes of its SHA-256 hash so open runs never see an existing file mutate).
  - Under `appcontainer`, `setUpAppContainer` grants `AccessReadExecute` to the
    selected `roots-*.pem` file on each run, and `quasidock prune` calls
    `roots.Prune` to delete older `roots-*.pem` files.
- **Verification**: [`tests/certs.sh`](../tests/certs.sh) generates an
  ephemeral test CA constrained to `localhost`, `127.0.0.0/8`, and `::1`, serves
  HTTPS and the CA's CRL (for Schannel/curl revocation checks), verifies every
  client refuses the untrusted cert, verifies `${roots}` readability across
  `soft`/`low`/`appcontainer` (including regenerating `roots-*.pem`), and—when
  elevated or run with `CERTS_ELEVATE=1`—adds the CA to `LocalMachine\Root` and
  checks all clients across `soft`, `low`, and `appcontainer` (tracking known
  `low`/`acprobe` gaps on Server 2025 such as `zig` at `low` and `jre`/`git`/
  `zig`/`composer` AppContainer gaps).

### Builds and out-of-scope items

- **Builds**: enforcing no network on `build` stages via AppContainer remains
  out of scope because several toolchains still fail inside AppContainer
  (e.g., `NUL`/device-path issues under Server 2025). Offline `build` stages
  continue to rely on the dead proxy convention.
- **Not in this design**: binding services on non-loopback interfaces (LAN),
  per-host network rules, or custom DNS.
