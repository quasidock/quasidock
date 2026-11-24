//go:build windows

package sdktools

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type probeResult struct {
	Args []string
	Cwd  string
}

func TestMain(m *testing.M) {
	if os.Getenv("QUASIDOCK_SDK_PROBE") == "1" {
		cwd, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(probeResult{os.Args[1:], cwd})
		if os.Getenv("QUASIDOCK_SDK_PROBE_FAIL") == "1" {
			os.Exit(23)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func copyProbe(t *testing.T, dest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("copy probe: %v / %v", err, closeErr)
	}
}

func TestPackagedSDKCommands(t *testing.T) {
	zig, err := exec.LookPath("zig")
	if err != nil {
		t.Skip("requires Zig to compile the packaged SDK executables")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	type command struct {
		name, target, variable string
		prefix                 []string
	}
	// Exercise each SDK's real command table with a process that reports the
	// arguments and working directory it actually received through Windows.
	cases := map[string][]command{
		"zig": {
			{"cc", "zig.exe", "", []string{"cc", "-target", "x86_64-windows-gnu"}},
			{"as", "zig.exe", "", []string{"cc", "-target", "x86_64-windows-gnu", "-c", "-x", "assembler"}},
			{"ar", "zig.exe", "", []string{"ar"}},
		},
		"llvm-mingw": {{"ar", `bin\llvm-ar.exe`, "", nil}, {"as", `bin\x86_64-w64-mingw32-clang.exe`, "", []string{"-c", "-x", "assembler"}}},
		"rust":       {{"dlltool", `lib\rustlib\x86_64-pc-windows-gnu\bin\self-contained\dlltool.exe`, "", []string{"--as", "as"}}},
		"maven":      {{"mvn", `runtime\java.exe`, "QUASIDOCK_MAVEN_JAVA", nil}},
		"gradle":     {{"gradle", `runtime\java.exe`, "QUASIDOCK_GRADLE_JAVA", nil}, {"gradle-vendor", `runtime\java.exe`, "QUASIDOCK_GRADLE_JAVA", []string{`%QUASIDOCK_SDK_DIR%\quasidock\GradleVendor.java`}}},
		"meson":      {{"meson", "python.exe", "", nil}, {"pkg-config", `bin\pkgconf.exe`, "", nil}, {"ninja", "ninja.exe", "", nil}},
		"composer":   {{"composer", `runtime\php.exe`, "QUASIDOCK_COMPOSER_PHP", nil}},
		"php-sdk":    {{"php-ext", "php.exe", "", []string{`%QUASIDOCK_SDK_DIR%\dev\quasidock\php-ext.php`}}},
		"make":       {{"gmake", `bin\make.exe`, "", nil}},
	}
	for sdk, commands := range cases {
		t.Run(sdk, func(t *testing.T) {
			work := t.TempDir()
			original := filepath.Join(work, "build")
			if err := os.MkdirAll(filepath.Join(original, "sdk-bin"), 0755); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(original, "sdk-bin", commands[0].name+".exe")
			build := exec.Command(zig, "cc", "-target", "x86_64-windows-gnu", "-O2", "-municode", "-s", "-Wl,-Brepro", "-Wall", "-Wextra", "-Werror", "-I", filepath.Join(root, "recipes", sdk), filepath.Join(root, "recipes", "sdk-tools", "launcher.c"), "-o", binary)
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("compile SDK commands: %v\n%s", err, out)
			}
			// Deploy under a different path containing spaces, Unicode and %.
			deploy := filepath.Join(work, "deployed SDK 日本語 % literal")
			if err := os.Rename(original, deploy); err != nil {
				t.Fatal(err)
			}
			first := filepath.Join(deploy, "sdk-bin", commands[0].name+".exe")
			for _, c := range commands {
				launcher := filepath.Join(deploy, "sdk-bin", c.name+".exe")
				if launcher != first {
					if err := os.Link(first, launcher); err != nil {
						t.Fatal(err)
					}
				}
				target := filepath.Join(deploy, c.target)
				if _, err := os.Stat(target); os.IsNotExist(err) {
					copyProbe(t, target)
				}
				args := []string{"", "two words", `a"b`, `C:\tail with space\`, "日本語", "%literal%"}
				if c.name == "as" {
					args = append([]string{"--64"}, args...)
				}
				run := exec.Command(launcher, args...)
				run.Dir = work
				run.Env = append(os.Environ(), "QUASIDOCK_SDK_PROBE=1")
				if c.variable != "" {
					run.Env = append(run.Env, c.variable+"="+target)
				}
				out, err := run.Output()
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				var got probeResult
				if err := json.Unmarshal(out, &got); err != nil {
					t.Fatalf("%s probe: %s: %v", c.name, out, err)
				}
				wantTail := args
				if c.name == "as" {
					wantTail = args[1:]
				}
				if len(got.Args) < len(wantTail) || !reflect.DeepEqual(got.Args[len(got.Args)-len(wantTail):], wantTail) {
					t.Fatalf("%s argument forwarding: got %#v, want tail %#v", c.name, got.Args, wantTail)
				}
				if c.prefix != nil {
					wantPrefix := make([]string, len(c.prefix))
					for i, arg := range c.prefix {
						wantPrefix[i] = strings.ReplaceAll(arg, "%QUASIDOCK_SDK_DIR%", deploy)
					}
					if !reflect.DeepEqual(got.Args[:len(got.Args)-len(wantTail)], wantPrefix) {
						t.Fatalf("%s prefix: got %#v, want %#v", c.name, got.Args, wantPrefix)
					}
				}
				for _, arg := range got.Args {
					if strings.Contains(arg, "%QUASIDOCK_") || strings.Contains(arg, original) {
						t.Fatalf("%s has an unresolved/build path: %q", c.name, arg)
					}
				}
				if got.Cwd != work {
					t.Fatalf("%s changed working directory: %s", c.name, got.Cwd)
				}
				failed := exec.Command(launcher, args...)
				failed.Dir = work
				failed.Env = append(run.Env, "QUASIDOCK_SDK_PROBE_FAIL=1")
				if err := failed.Run(); err == nil || failed.ProcessState.ExitCode() != 23 {
					t.Fatalf("%s did not preserve child exit status: %v", c.name, err)
				}
				if c.variable != "" {
					missing := exec.Command(launcher)
					missing.Env = append(run.Env, c.variable+"=")
					if err := missing.Run(); err == nil || missing.ProcessState.ExitCode() != 127 {
						t.Fatalf("%s did not reject an unset runtime path: %v", c.name, err)
					}
				}
			}
		})
	}
}
