# NGINX

[The NGINX recipe](../recipes/nginx.json) builds native Windows x64 NGINX
1.31.10 with LLVM-MinGW, static PCRE2 10.49, zlib 1.4, and OpenSSL 3.5.9.
It includes HTTP, HTTPS, HTTP/2, reverse proxying, FastCGI, rewrite, and gzip.
Build tools are SDK dependencies and are not shipped in the runtime image.
The image is local until its first publication; build it before running it.

```powershell
go run ./scripts/build.go
.\out\bin\quasidock.exe build recipes/nginx.json
.\out\bin\quasidock.exe run --rm --entrypoint=nginx nginx -v
.\out\bin\quasidock.exe run -d --name web --port=http=8080 nginx
curl.exe http://127.0.0.1:8080/
.\out\bin\quasidock.exe stop web
.\out\bin\quasidock.exe rm web
```

The default sandbox is `low`; `soft` and `appcontainer` are also supported.
The `http` and `https` services default to 8080 and 8443. Use
`--port=http=PORT` and `--port=https=PORT` to change them, including when running
multiple containers. Listeners bind to loopback. Under AppContainer, the
launcher uses Quasidock's internal listen ports, and Quasidock publishes the
host ports through its relay.

## Configuration and content

The `bin/nginx.exe` entrypoint is a small native launcher. It runs the upstream
`libexec/nginx.exe`, waits for it, and returns its exit code. Informational
options (`-v`, `-V`, `-h`, `-?`) run without preparing configuration. Normal
startup and `-t`/`-T` generate `${data}/conf/nginx.conf` from the image template.
That file is managed by the launcher: use snippets or `-c` for customizations.
Signal commands (`-s`) reuse the saved file without regenerating it.

| Setting | Default / behavior |
| --- | --- |
| `NGINX_DATA` | `${data}`; configuration, PID, log, and temporary file root |
| `NGINX_DOCUMENT_ROOT` | `${app}/html`; bundled default page |
| `NGINX_HTTP_PORT` | `${listen:http}`; normally configured with `--port=http=...` |
| `NGINX_HTTPS_PORT` | `${listen:https}`; normally configured with `--port=https=...` |
| `NGINX_TLS_CERT_FILE` | Optional PEM certificate path; enables HTTPS with the key |
| `NGINX_TLS_KEY_FILE` | Optional PEM private-key path; both TLS settings are required together |

The generated server includes `${data}/conf.d/*.conf` in **server context**.
Use these snippets for locations, rewrites, response headers, and upstream
proxy directives. Gzip is enabled for HTML, plain text, CSS, JavaScript, and
JSON. Dotfiles are denied. NGINX variables such as `$request_uri` in snippets
are passed through unchanged.

For persistent content, mount the data tree and supply the document root's
absolute path. Environment overrides supplied with `-e` are literal values.
Mounted storage and the Quasidock store must be on local NTFS volumes.
Create an `index.html` in the `www` directory before starting the server:

```powershell
$dataPath = Join-Path $env:LOCALAPPDATA 'QuasidockNginx'
New-Item -ItemType Directory -Force "$dataPath\www", "$dataPath\conf.d"
.\out\bin\quasidock.exe run -d --name web `
  --mount "type=bind,source=$dataPath,target=`${data}" `
  -e "NGINX_DOCUMENT_ROOT=$dataPath\www" nginx
```

Configuration paths use forward slashes internally and support spaces and
Unicode. Paths supplied for generation cannot contain control characters or
`$`, which NGINX interprets as a configuration variable. The launcher quotes
path values and substitutes only its own template tokens.

Logs are `${data}/logs/error.log` and `${data}/logs/access.log`; the PID file is
`${data}/logs/nginx.pid`. Runtime output is also available through
`quasidock logs`, but request logs remain in data files. Mount the data tree to
retain them after container removal. Image files stay unchanged.

## TLS and custom configuration

Supply an existing PEM certificate and key, stored in mounted data or another
granted directory. The generated server keeps HTTP enabled and adds HTTPS with
TLS 1.2/1.3 and HTTP/2. No certificate or private key is generated or shipped in
the image. A missing setting or invalid credential fails startup.

```powershell
.\out\bin\quasidock.exe run -d --name web-tls `
  --mount "type=bind,source=$dataPath,target=`${data}" `
  -e "NGINX_TLS_CERT_FILE=$dataPath\server.crt" `
  -e "NGINX_TLS_KEY_FILE=$dataPath\server.key" nginx
```

Use `--entrypoint=nginx` when passing NGINX options directly after the image:

```powershell
.\out\bin\quasidock.exe run --rm --entrypoint=nginx nginx -t
.\out\bin\quasidock.exe run --rm --entrypoint=nginx nginx -T
```

`-c FILE` selects a complete user-managed configuration; the launcher does not
rewrite or interpolate that file. The default prefix is `${data}/`; `-p DIR`
overrides it, and relative `-c` paths resolve against that prefix. Native `-g`
and `-e` options are forwarded. Custom configurations should set `daemon off;`
so Windows console shutdown remains available, use Quasidock's assigned listen
ports, and keep writable paths inside data. The generated configuration uses
one worker, reflecting the native Windows server's effective worker limit.

## Lifecycle and verification

```powershell
.\out\bin\quasidock.exe exec web nginx.exe -t
.\out\bin\quasidock.exe exec web nginx.exe -s reload
.\out\bin\quasidock.exe exec web nginx.exe -s reopen
.\out\bin\quasidock.exe restart web
.\out\bin\quasidock.exe exec web nginx.exe -s quit
```

For custom configurations, supply the same `-c`/`-p` settings to `exec` commands.
`-s stop` requests fast shutdown and `-s quit` requests graceful shutdown.
Container stop/restart also deliver the Windows console control event and wait
for shutdown, then terminate the process tree if the timeout expires.

Run the integration suite after building the image:

```bash
NGINX_HOME=path/to/store bash tests/nginx.sh
```

The suite requires Windows, Git Bash, Go, curl, and OpenSSL. It verifies TLS
with a temporary test certificate and checks HTTP/2, proxy/FastCGI traffic,
configuration, lifecycle, and isolation in all three sandboxes. The dedicated
Reproduce CI job also compares builds from two independent stores. TCP stream
proxying, mail modules, third-party modules, and HTTP/3 are outside this image.
