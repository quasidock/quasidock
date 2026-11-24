package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
)

type Fetcher struct {
	ctx     *context.Context
	baseDir string
	app     string
	mirrors []string
}

func NewFetcher(ctx *context.Context, baseDir, app string, mirrors ...string) *Fetcher {
	return &Fetcher{
		ctx:     ctx,
		baseDir: baseDir,
		app:     app,
		mirrors: mirrors,
	}
}

func (f *Fetcher) Fetch(src recipe.Source) (string, error) {
	if src.Path != nil {
		full := *src.Path
		if !filepath.IsAbs(full) {
			full = filepath.Join(f.baseDir, full)
		}
		if src.Sha256 != nil {
			got, err := store.Sha256File(full)
			if err != nil {
				return "", fmt.Errorf("reading %s: %w", full, err)
			}
			if !strings.EqualFold(*src.Sha256, got.Hex) {
				return "", fmt.Errorf("sha256 mismatch for %s\n  expected %s\n  got      %s", full, *src.Sha256, got.Hex)
			}
		}
		return full, nil
	}

	url := *src.URL
	if src.Sha256 != nil {
		cached := f.ctx.Store.Path("cache", "downloads", *src.Sha256)
		if store.Exists(cached) {
			return cached, nil
		}
	}

	tmp := f.ctx.Store.TmpPath("download")
	defer func() { _ = os.Remove(tmp) }()

	f.ctx.Note("fetching %s", url)
	if err := downloadFile(url, tmp); err != nil {
		return f.fallback(src, fmt.Errorf("downloading %s: %w", url, err))
	}

	got, err := store.Sha256File(tmp)
	if err != nil {
		return "", err
	}

	cached := f.ctx.Store.Path("cache", "downloads", got.Hex)
	_ = os.MkdirAll(filepath.Dir(cached), 0755)
	if err := store.RenameRetrying(tmp, cached, 5000); err != nil {
		return "", err
	}

	if src.Sha256 == nil {
		return "", fmt.Errorf("source %s has no sha256. Pin it with:\n  \"sha256\": %q", url, got.Hex)
	}
	if !strings.EqualFold(*src.Sha256, got.Hex) {
		return f.fallback(src, fmt.Errorf("%s: it's no longer the pinned file (expected %s, got %s)", url, *src.Sha256, got.Hex))
	}
	return cached, nil
}

func (f *Fetcher) fallback(src recipe.Source, cause error) (string, error) {
	if src.Sha256 == nil || (f.app == "" && len(f.mirrors) == 0) {
		return "", cause
	}
	images := []string{}
	if f.app != "" {
		images = append(images, f.app)
	}
	images = append(images, f.mirrors...)
	var failures []string
	for _, image := range images {
		path, err := remote.FetchSource(f.ctx.Store, f.ctx.Env[remote.RegistryVar], image, *src.Sha256, f.ctx.Verbose)
		if err == nil {
			return path, nil
		}
		failures = append(failures, fmt.Sprintf("sources next to %s: %v", image, err))
	}
	return "", fmt.Errorf("%w; %s", cause, strings.Join(failures, "; "))
}

func (f *Fetcher) FetchAll(srcs []recipe.Source) error {
	var todo []recipe.Source
	pinned := make(map[string]bool)

	for _, src := range srcs {
		if src.Path != nil {
			if _, err := f.Fetch(src); err != nil {
				return err
			}
			continue
		}
		if src.Sha256 != nil {
			cached := f.ctx.Store.Path("cache", "downloads", *src.Sha256)
			if store.Exists(cached) {
				continue
			}
			if pinned[*src.Sha256] {
				continue
			}
			pinned[*src.Sha256] = true
		}
		todo = append(todo, src)
	}

	if len(todo) <= 1 {
		for _, src := range todo {
			if _, err := f.Fetch(src); err != nil {
				return err
			}
		}
		return nil
	}

	numWorkers := 8
	if len(todo) < numWorkers {
		numWorkers = len(todo)
	}
	if f.ctx.Verbose {
		f.ctx.Note("fetch %d files, %d at a time", len(todo), numWorkers)
	}

	var nextIdx atomic.Uint64
	var firstErr error
	var errMu sync.Mutex

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(nextIdx.Add(1) - 1)
				if i >= len(todo) {
					return
				}
				if _, err := f.Fetch(todo[i]); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					nextIdx.Store(uint64(len(todo)))
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func downloadFile(url, dest string) error {
	resp, err := http.Get(url) //nolint:gosec // G107: fetches recipe source URL verified against pinned SHA-256
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	if _, err := io.Copy(mw, resp.Body); err != nil {
		_ = f.Close()
		return err
	}
	_ = hex.EncodeToString(hasher.Sum(nil))
	return f.Close()
}
