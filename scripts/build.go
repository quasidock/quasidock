package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/version"
	"github.com/quasidock/quasidock/internal/winres"
)

type binaryTarget struct {
	pkg  string
	exe  string
	gui  bool
	desc string
}

var productionTargets = []binaryTarget{
	{pkg: "./cmd/quasidock", exe: "quasidock.exe", gui: false, desc: "quasidock"},
	{pkg: "./cmd/quasidock-relay", exe: "quasidock-relay.exe", gui: false, desc: "quasidock service relay"},
	{pkg: "./cmd/quasidock-host", exe: "quasidock-host.exe", gui: false, desc: "quasidock host command"},
}

var testTargetMap = map[string]binaryTarget{
	"cli-fixture":  {pkg: "./tests/cmd/clifixture", exe: "quasidock-cli-fixture.exe", gui: false},
	"ctrlc-driver": {pkg: "./tests/cmd/ctrlc", exe: "quasidock-ctrlc.exe", gui: false},
	"argv-echo":    {pkg: "./tests/cmd/argv", exe: "quasidock-argv.exe", gui: false},
	"acprobe":      {pkg: "./tests/cmd/acprobe", exe: "quasidock-acprobe.exe", gui: false},
	"netprobe":     {pkg: "./tests/cmd/netprobe", exe: "quasidock-netprobe.exe", gui: false},
	"hostprobe":    {pkg: "./tests/cmd/hostprobe", exe: "quasidock-hostprobe.exe", gui: false},
	"regprobe":     {pkg: "./tests/cmd/regprobe", exe: "quasidock-regprobe.exe", gui: false},
	"resetproxy":   {pkg: "./tests/cmd/resetproxy", exe: "quasidock-resetproxy.exe", gui: false},
	"p256sign":     {pkg: "./tests/cmd/p256sign", exe: "quasidock-p256sign.exe", gui: false},
	"winwait":      {pkg: "./tests/cmd/winwait", exe: "quasidock-winwait.exe", gui: false},
	"procprobe":    {pkg: "./tests/cmd/procprobe", exe: "quasidock-procprobe.exe", gui: false},
	"jobprobe":     {pkg: "./tests/cmd/jobprobe", exe: "quasidock-jobprobe.exe", gui: false},
	"asuser":       {pkg: "./tests/cmd/asuser", exe: "quasidock-asuser.exe", gui: false},
	"imports":      {pkg: "./tests/cmd/imports", exe: "quasidock-imports.exe", gui: false},
	"cred-helper":  {pkg: "./tests/cmd/credhelper", exe: "docker-credential-quasidock-test.exe", gui: false},
	"htpasswd":     {pkg: "./tests/cmd/htpasswd", exe: "quasidock-htpasswd.exe", gui: false},
}

const manifestXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
    </application>
  </compatibility>
</assembly>`

func main() {
	prefix := "out"
	strip := false
	var targets []string
	var passthru []string
	isRun := false

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-p" || arg == "--prefix" {
			if i+1 < len(args) {
				prefix = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "-p=") || strings.HasPrefix(arg, "--prefix=") {
			prefix = arg[strings.Index(arg, "=")+1:]
		} else if arg == "--strip" {
			strip = true
		} else if arg == "run" {
			isRun = true
			if i+1 < len(args) {
				passthru = args[i+1:]
			}
			break
		} else if arg == "test" {
			targets = append(targets, "test")
		} else if strings.HasPrefix(arg, "--summary") {
			// ignore summary flags like --summary all
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		} else if strings.HasPrefix(arg, "-") {
			// ignore unrecognized options
		} else {
			targets = append(targets, arg)
		}
	}

	binDir := filepath.Join(prefix, "bin")
	testDir := filepath.Join(prefix, "test")

	// If run target is requested
	if isRun {
		exePath := filepath.Join(binDir, "quasidock.exe")
		if _, err := os.Stat(exePath); err != nil {
			// Build quasidock first
			if err := buildBinary(productionTargets[0], binDir, strip); err != nil {
				fmt.Fprintf(os.Stderr, "failed to build quasidock: %v\n", err)
				os.Exit(1)
			}
		}
		cmd := exec.Command(exePath, passthru...) //nolint:gosec // G204: build.go intentionally executes the built binary
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.ExitCode())
			}
			os.Exit(1)
		}
		return
	}

	// If no targets specified, build all production targets
	if len(targets) == 0 {
		for _, t := range productionTargets {
			if err := buildBinary(t, binDir, strip); err != nil {
				fmt.Fprintf(os.Stderr, "failed to build %s: %v\n", t.exe, err)
				os.Exit(1)
			}
		}
		return
	}

	// Process specified targets
	for _, targetName := range targets {
		if targetName == "test" {
			cmd := exec.Command("go", "test", "./...")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOAMD64=v1")
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "tests failed: %v\n", err)
				os.Exit(1)
			}
			continue
		}

		if targetName == "all" {
			for _, t := range productionTargets {
				if err := buildBinary(t, binDir, strip); err != nil {
					fmt.Fprintf(os.Stderr, "failed to build %s: %v\n", t.exe, err)
					os.Exit(1)
				}
			}
			continue
		}

		// Check if it's a test target
		if t, ok := testTargetMap[targetName]; ok {
			if err := buildBinary(t, testDir, strip); err != nil {
				fmt.Fprintf(os.Stderr, "failed to build %s: %v\n", t.exe, err)
				os.Exit(1)
			}
			continue
		}

		// Check if it matches any production target by name
		foundProd := false
		for _, t := range productionTargets {
			nameWithoutExt := strings.TrimSuffix(t.exe, ".exe")
			if targetName == t.exe || targetName == nameWithoutExt {
				foundProd = true
				if err := buildBinary(t, binDir, strip); err != nil {
					fmt.Fprintf(os.Stderr, "failed to build %s: %v\n", t.exe, err)
					os.Exit(1)
				}
				break
			}
		}

		if !foundProd {
			fmt.Fprintf(os.Stderr, "unknown target: %s\n", targetName)
			os.Exit(1)
		}
	}
}

func buildBinary(t binaryTarget, destDir string, strip bool) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	outPath := filepath.Join(destDir, t.exe)

	if t.desc != "" {
		sysoPath := filepath.Join(t.pkg, "rsrc_windows_amd64.syso")
		err := winres.WriteSYSO(sysoPath, winres.Params{
			Version:     version.Version,
			Description: t.desc,
			FileName:    t.exe,
			ManifestXML: []byte(manifestXML),
		})
		if err == nil {
			defer func() { _ = os.Remove(sysoPath) }()
		}
	}

	var ldflags []string
	if t.gui {
		ldflags = append(ldflags, "-H=windowsgui")
	}
	if strip {
		ldflags = append(ldflags, "-s", "-w")
	}

	buildArgs := []string{"build"}
	if strip {
		buildArgs = append(buildArgs, "-trimpath")
	}
	if len(ldflags) > 0 {
		buildArgs = append(buildArgs, fmt.Sprintf("-ldflags=%s", strings.Join(ldflags, " ")))
	}
	buildArgs = append(buildArgs, "-o", outPath, t.pkg)

	cmd := exec.Command("go", buildArgs...) //nolint:gosec // G702: build.go intentionally invokes go build with target flags
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOAMD64=v1")
	return cmd.Run()
}
