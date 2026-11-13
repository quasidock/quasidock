package lifecycle

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/process"
	"github.com/zigsaw-io/zigsaw/internal/prompt"
	"github.com/zigsaw-io/zigsaw/internal/remote"
	"github.com/zigsaw-io/zigsaw/internal/shortcuts"
	"github.com/zigsaw-io/zigsaw/internal/store"
	"github.com/zigsaw-io/zigsaw/internal/trust"
	"github.com/zigsaw-io/zigsaw/internal/version"
	"github.com/zigsaw-io/zigsaw/internal/win32"
	"github.com/zigsaw-io/zigsaw/internal/zipfile"
)

const defaultUpgradeFrom = "https://github.com/zigsaw-io/zigsaw/releases/latest/download"
const upgradeArchive = "zigsaw-x86_64-windows.zip"
const reinstallCmd = "irm https://github.com/zigsaw-io/zigsaw/releases/latest/download/install.ps1 | iex"

var upgradePrograms = []string{
	"zigsaw-shim.exe",
	"zigsaw-shimw.exe",
	"zigsaw-relay.exe",
	"zigsaw-host.exe",
	"zigsaw-notify.exe",
	"zigsaw.exe",
}

func upgradeFrom(ctx *context.Context) string {
	if ctx != nil && ctx.Env != nil {
		if val := ctx.Env["ZIGSAW_UPGRADE_FROM"]; val != "" {
			return strings.TrimRight(val, "/\\")
		}
	}
	if val := os.Getenv("ZIGSAW_UPGRADE_FROM"); val != "" {
		return strings.TrimRight(val, "/\\")
	}
	return defaultUpgradeFrom
}

func isURL(where string) bool {
	return strings.HasPrefix(where, "https://") || strings.HasPrefix(where, "http://")
}

func Applies(ctx *context.Context) bool {
	if ctx != nil && ctx.Env != nil && ctx.Env["ZIGSAW_UPGRADE_FROM"] != "" {
		return true
	}
	if os.Getenv("ZIGSAW_UPGRADE_FROM") != "" {
		return true
	}
	return inStoreBin(ctx)
}

func inStoreBin(ctx *context.Context) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	selfDir := filepath.Dir(self)
	storeBin := ctx.Store.Path("bin")
	return strings.EqualFold(selfDir, storeBin)
}

func parseSemver(v string) (int, int, int, bool) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, false
	}
	return maj, min, patch, true
}

func IsNewer(ver string) bool {
	tMaj, tMin, tPatch, ok1 := parseSemver(ver)
	oMaj, oMin, oPatch, ok2 := parseSemver(version.Version)
	if !ok1 || !ok2 {
		return false
	}
	if tMaj != oMaj {
		return tMaj > oMaj
	}
	if tMin != oMin {
		return tMin > oMin
	}
	return tPatch > oPatch
}

func ListedHash(sums, name string) *string {
	lines := strings.Split(sums, "\n")
	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash := fields[0]
		fileName := strings.TrimPrefix(fields[1], "*")
		if len(fields) != 2 || fileName != name {
			continue
		}
		if len(hash) != 64 {
			continue
		}
		isHex := true
		for i := 0; i < len(hash); i++ {
			c := hash[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				isHex = false
				break
			}
		}
		if !isHex {
			return nil
		}
		lower := strings.ToLower(hash)
		return &lower
	}
	return nil
}

// Newer returns the latest version if newer than current and properly signed.
func Newer(ctx *context.Context) (string, error) {
	latest, err := latestVersion(ctx)
	if err != nil {
		return "", err
	}
	if !IsNewer(latest) {
		return "", nil
	}
	release, err := readSums(ctx, latest)
	if err != nil {
		return "", err
	}
	if release.state != sumsSigned {
		return "", nil
	}
	return latest, nil
}

func Upgrade(ctx *context.Context, consent remote.Consent) (uint32, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}
	bin := filepath.Dir(self)
	if !inStoreBin(ctx) {
		return 1, fmt.Errorf("this zigsaw, %s, isn't in its store's bin, so upgrade won't replace it. To install the latest one, in PowerShell:\n  %s", self, reinstallCmd)
	}

	latest, err := latestVersion(ctx)
	if err != nil {
		return 1, err
	}
	if !IsNewer(latest) {
		ctx.Note("zigsaw %s is up to date (the latest release is %s)", version.Version, latest)
		return 0, nil
	}

	release, err := readSums(ctx, latest)
	if err != nil {
		return 1, err
	}
	if release.state == sumsNotSignedYet {
		return 1, fmt.Errorf("zigsaw %s is out, but isn't signed yet, so zigsaw stays at %s. Nothing was changed; try again later", latest, version.Version)
	}

	if consent != remote.ConsentGiven {
		ans, err := prompt.AskYesNo(ctx, fmt.Sprintf("Upgrade zigsaw %s to %s?", version.Version, latest))
		if err != nil {
			return 1, err
		}
		switch ans {
		case prompt.AnswerYes:
			// proceed
		case prompt.AnswerNo:
			return 1, fmt.Errorf("zigsaw stays at %s", version.Version)
		case prompt.AnswerNone:
			return 1, fmt.Errorf("zigsaw stays at %s: nothing answered whether to go on. Run it at a console, or add --yes", version.Version)
		}
	}
	deleteLeftovers(bin)

	tmp, err := ctx.Store.MakeTmpDir("upgrade")
	if err != nil {
		return 1, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	zipPath := filepath.Join(tmp, upgradeArchive)
	if err := getUpgradeFile(ctx, upgradeArchive, zipPath); err != nil {
		return 1, err
	}

	want := ListedHash(release.text, upgradeArchive)
	if want == nil {
		return 1, fmt.Errorf("SHA256SUMS from %s doesn't list %s. Nothing was changed", upgradeFrom(ctx), upgradeArchive)
	}
	got, err := store.Sha256File(zipPath)
	if err != nil || !strings.EqualFold(*want, got.Hex) {
		return 1, fmt.Errorf("%s from %s isn't the file its SHA256SUMS lists: expected %s, got %s. Nothing was changed", upgradeArchive, upgradeFrom(ctx), *want, got.Hex)
	}

	var randBytes [4]byte
	_, _ = rand.Read(randBytes[:])
	suffix := hex.EncodeToString(randBytes[:])

	var newPaths []string
	cleanUnpacked := func() {
		for _, p := range newPaths {
			_ = os.Remove(p)
		}
	}
	defer cleanUnpacked()

	zipF, err := os.Open(zipPath)
	if err != nil {
		return 1, err
	}
	fi, _ := zipF.Stat()
	entries, err := zipfile.List(zipF, fi.Size())
	if err != nil {
		_ = zipF.Close()
		return 1, fmt.Errorf("reading %s: %w. Nothing was changed", upgradeArchive, err)
	}

	for _, name := range upgradePrograms {
		var found *zipfile.Entry
		for i := range entries {
			if entries[i].Name == name {
				found = &entries[i]
				break
			}
		}
		if found == nil {
			_ = zipF.Close()
			return 1, fmt.Errorf("%s has no %s. Nothing was changed", upgradeArchive, name)
		}
		rc, err := found.Open()
		if err != nil {
			_ = zipF.Close()
			return 1, err
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			_ = zipF.Close()
			return 1, err
		}
		outPath := filepath.Join(bin, fmt.Sprintf("%s.new-%s", name, suffix))
		if err := os.WriteFile(outPath, data, 0755); err != nil {
			_ = zipF.Close()
			return 1, err
		}
		newPaths = append(newPaths, outPath)
	}
	_ = zipF.Close()

	newZigsaw := newPaths[len(upgradePrograms)-1]
	res, err := process.Output(process.SpawnSpec{
		Exe:         newZigsaw,
		CommandLine: process.BuildCommandLine(newZigsaw, []string{"version"}),
	}, nil, 60*1000)
	if err != nil {
		return 1, fmt.Errorf("the new zigsaw wouldn't start here (%w); if Windows Security says it found a threat, that's why. Nothing was changed", err)
	}
	expectedOutput := fmt.Sprintf("zigsaw %s", latest)
	outStr := strings.TrimSpace(string(res.Stdout))
	if res.Code != 0 || outStr != expectedOutput {
		return 1, fmt.Errorf("the new zigsaw says %q, but this release is %s. Nothing was changed", outStr, latest)
	}

	if err := swapPrograms(bin, newPaths, suffix); err != nil {
		return 1, err
	}
	newPaths = nil // Successfully swapped, don't delete!
	ctx.Note("upgraded zigsaw %s to %s", version.Version, latest)

	installed := filepath.Join(bin, "zigsaw.exe")
	repaired, _ := process.Spawn(process.SpawnSpec{
		Exe:         installed,
		CommandLine: process.BuildCommandLine(installed, []string{"repair"}),
	})
	if repaired != 0 {
		ctx.Note("warning: some apps' commands are still those of the zigsaw before. Close the apps and run `zigsaw repair`")
	}

	localAppData := os.Getenv("LOCALAPPDATA")
	if ctx != nil && ctx.Env != nil && ctx.Env["LOCALAPPDATA"] != "" {
		localAppData = ctx.Env["LOCALAPPDATA"]
	}
	if shortcuts.IsDefaultStore(localAppData, ctx.Store.Root) {
		_, _ = process.Spawn(process.SpawnSpec{
			Exe:         installed,
			CommandLine: process.BuildCommandLine(installed, []string{"updates", "on", "--unless-off"}),
		})
	}
	return 0, nil
}

type sumsState int

const (
	sumsSigned sumsState = iota
	sumsNotSignedYet
)

type releaseSums struct {
	state sumsState
	text  string
}

func readSums(ctx *context.Context, ver string) (releaseSums, error) {
	sumsPath := ctx.Store.TmpPath("sums")
	sigPath := ctx.Store.TmpPath("sums-sig")
	defer func() {
		_ = os.Remove(sumsPath)
		_ = os.Remove(sigPath)
	}()

	hasSig, err := getUpgradeFileIfThere(ctx, "SHA256SUMS.sig", sigPath)
	if err != nil {
		return releaseSums{}, err
	}
	if !hasSig {
		return releaseSums{state: sumsNotSignedYet}, nil
	}

	if err := getUpgradeFile(ctx, "SHA256SUMS", sumsPath); err != nil {
		return releaseSums{}, err
	}

	sumsData, err := os.ReadFile(sumsPath)
	if err != nil {
		return releaseSums{}, err
	}
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		return releaseSums{}, err
	}

	keys, err := trust.ForRelease(ctx.Store)
	if err != nil {
		return releaseSums{}, err
	}
	for _, key := range keys {
		if key.Verifies(string(sumsData), string(sigData)) {
			listed := ListedHash(string(sumsData), "VERSION")
			if listed == nil {
				return releaseSums{}, fmt.Errorf("the signed SHA256SUMS from %s doesn't list VERSION. Nothing was changed", upgradeFrom(ctx))
			}
			h := sha256.Sum256([]byte(ver + "\n"))
			if !strings.EqualFold(*listed, hex.EncodeToString(h[:])) {
				return releaseSums{}, fmt.Errorf("the VERSION from %s says %s, which isn't what the release's signed SHA256SUMS lists. Nothing was changed", upgradeFrom(ctx), ver)
			}
			return releaseSums{state: sumsSigned, text: string(sumsData)}, nil
		}
	}
	return releaseSums{}, fmt.Errorf("the signature of SHA256SUMS from %s isn't by any key this zigsaw knows. Nothing was changed", upgradeFrom(ctx))
}

func latestVersion(ctx *context.Context) (string, error) {
	tmp := ctx.Store.TmpPath("version")
	defer func() { _ = os.Remove(tmp) }()
	if err := getUpgradeFile(ctx, "VERSION", tmp); err != nil {
		return "", err
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(data))
	if _, _, _, ok := parseSemver(text); !ok {
		return "", fmt.Errorf("%s/VERSION isn't a version: %q", upgradeFrom(ctx), text)
	}
	return text, nil
}

func getUpgradeFile(ctx *context.Context, name, dest string) error {
	found, err := getUpgradeFileIfThere(ctx, name, dest)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("downloading %s/%s: HTTP 404", upgradeFrom(ctx), name)
	}
	return nil
}

func getUpgradeFileIfThere(ctx *context.Context, name, dest string) (bool, error) {
	where := upgradeFrom(ctx)
	if !isURL(where) {
		src := filepath.Join(where, name)
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, fmt.Errorf("reading %s: %w", src, err)
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return false, err
		}
		return true, nil
	}

	url := fmt.Sprintf("%s/%s", where, name)
	resp, err := http.Get(url)
	if err != nil {
		return false, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return false, err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return false, err
	}
	return true, nil
}

type swappedProg struct {
	dest  string
	aside string
	new   string
}

func swapPrograms(bin string, newPaths []string, suffix string) error {
	var done []swappedProg
	rollbackSwaps := func() {
		for i := len(done) - 1; i >= 0; i-- {
			d := done[i]
			_ = win32Move(d.dest, d.new)
			if d.aside != "" {
				_ = win32Move(d.aside, d.dest)
			}
		}
	}

	for i, name := range upgradePrograms {
		newPath := newPaths[i]
		dest := filepath.Join(bin, name)
		aside := fmt.Sprintf("%s.old-%s", dest, suffix)
		there := store.Exists(dest)

		if there {
			if err := win32Move(dest, aside); err != nil {
				rollbackSwaps()
				return fmt.Errorf("moving %s aside: %w", name, err)
			}
		}
		if err := win32Move(newPath, dest); err != nil {
			if there {
				_ = win32Move(aside, dest)
			}
			rollbackSwaps()
			return fmt.Errorf("putting new %s in place: %w", name, err)
		}
		movedAside := ""
		if there {
			movedAside = aside
		}
		done = append(done, swappedProg{
			dest:  dest,
			aside: movedAside,
			new:   newPath,
		})
	}
	return nil
}

func win32Move(src, dst string) error {
	return win32.MoveFileEx(src, dst, 0)
}

func deleteLeftovers(bin string) {
	entries, err := os.ReadDir(bin)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		for _, prog := range upgradePrograms {
			if strings.HasPrefix(name, prog) {
				rest := name[len(prog):]
				if strings.HasPrefix(rest, ".old-") || strings.HasPrefix(rest, ".new-") {
					_ = syscall.Unlink(filepath.Join(bin, name))
				}
			}
		}
	}
}
