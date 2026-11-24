package winres

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWinres(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("testdata", "quasidock.manifest"))
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}

	tmpDir, err := os.MkdirTemp("", "winres-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	sysoPath := filepath.Join(tmpDir, "rsrc_windows_amd64.syso")
	err = WriteSYSO(sysoPath, Params{
		Version:     "0.78.0",
		Description: "quasidock test binary",
		FileName:    "testbin.exe",
		ManifestXML: manifest,
	})
	if err != nil {
		t.Fatalf("WriteSYSO: %v", err)
	}

	mainGo := filepath.Join(tmpDir, "main.go")
	err = os.WriteFile(mainGo, []byte("package main\nfunc main() {}\n"), 0666)
	if err != nil {
		t.Fatalf("writing main.go: %v", err)
	}

	err = os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module testbin\n\ngo 1.26\n"), 0666)
	if err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}

	exePath := filepath.Join(tmpDir, "testbin.exe")
	cmd := exec.Command("go", "build", "-o", exePath, ".")
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH=amd64")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\nOutput: %s", err, string(out))
	}

	// Verify using PowerShell Get-Item VersionInfo
	psCmd := exec.Command("powershell", "-NoProfile", "-Command",
		"$v = (Get-Item -LiteralPath '"+exePath+"').VersionInfo; $v.ProductVersion + '|' + $v.OriginalFilename")
	psOut, err := psCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("powershell: %v\nOutput: %s", err, string(psOut))
	}
	info := strings.TrimSpace(string(psOut))
	expected := "0.78.0|testbin.exe"
	if info != expected {
		t.Errorf("expected VersionInfo %q, got %q", expected, info)
	}

	// Verify manifest presence
	exeBytes, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("reading built exe: %v", err)
	}
	if !strings.Contains(string(exeBytes), `level="asInvoker"`) {
		t.Errorf("manifest not found in built binary")
	}
}
