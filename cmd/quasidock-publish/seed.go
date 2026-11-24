package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/publishing"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
)

// The seed tag is separate from version/latest tags. Only the recipe's pinned
// source hash is trusted; fetching the seed never contacts the upstream URL.
func seedBusybox(ctx *context.Context, r publishing.Recipe, local string) (string, error) {
	parsed, err := recipe.Parse(r.File, r.Bytes)
	if err != nil {
		return "", err
	}
	if parsed.ID != "busybox" || len(parsed.Stages) != 1 || len(parsed.Stages[0].Sources) != 1 {
		return "", fmt.Errorf("expected the single-source BusyBox recipe")
	}
	src := parsed.Stages[0].Sources[0]
	if src.URL == nil || src.Sha256 == nil {
		return "", fmt.Errorf("BusyBox source must have a URL and SHA-256")
	}
	hash := *src.Sha256
	cached := ctx.Store.Path("cache", "downloads", hash)
	if local == "" {
		if got, err := store.Sha256File(cached); err == nil && got.Hex == hash {
			return cached, nil
		}
		var failures []error
		for _, image := range append([]string{"busybox:seed-" + hash}, parsed.SourceMirrors...) {
			path, err := remote.FetchSource(ctx.Store, ctx.Env[remote.RegistryVar], image, hash, ctx.Verbose)
			if err == nil {
				return path, nil
			}
			failures = append(failures, err)
		}
		return "", fmt.Errorf("seeding BusyBox: %w", errors.Join(failures...))
	}
	in, err := os.Open(local)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	tmp := ctx.Store.TmpPath("busybox-seed")
	defer func() { _ = os.Remove(tmp) }()
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	got, err := store.Sha256File(tmp)
	if err != nil {
		return "", err
	}
	if got.Hex != hash {
		return "", fmt.Errorf("BusyBox SHA-256 mismatch: expected %s, got %s", hash, got.Hex)
	}
	if err := store.RenameRetrying(tmp, cached, 5000); err != nil {
		return "", err
	}
	return cached, nil
}
