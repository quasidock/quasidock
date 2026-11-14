# Quasidock - native Windows container solution

Quasidock is a Windows-native Docker-compatible container solution.

It started at a hard fork of [Zigsaw](https://github.com/zigsaw-io/zigsaw), ported to Go, keeping only with the Docker/OCI parts; and add a Docker CLI compatible interface.

# How it works (to be updated)

On Windows there is no simple and lightweigh container primitives like Linux.
- Hyper-V requires Windows Pro
- Windows Containers is only available for Windows Server
- WSL is not that lightweight, and using Linux binary has both it upside and downside

Quasidock ultilizes technologies that available on most Windows machine (especially Windows Home Edition) like AppContainer, HCS (Host Compute Service), Cloud Files API, etc. It is not quite well-isolated as Linux containers or VM-based solutions (hence, "quasi"), but Quasidock should provide much more lightweight and smoother experience.

# Building

```sh
go run ./scripts/build.go
```

Binaries are written to `out/bin/quasidock.exe`, `out/bin/quasidock-relay.exe`, and `out/bin/quasidock-host.exe`. Pass `--strip` to omit debug symbols (`-trimpath -ldflags="-s -w"`). To run all Go unit tests:

```sh
go test ./...
```
