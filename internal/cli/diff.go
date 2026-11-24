package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
)

func diffCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) != 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return 1, errors.New("usage: quasidock diff CONTAINER")
	}
	paths, err := ctx.Store.ContainerDiff(args[0])
	if err != nil {
		return 1, err
	}
	for _, path := range paths {
		if _, err := fmt.Fprintf(ctx.Stdout, "%c %s\n", path.Kind, path.Path); err != nil {
			return 1, err
		}
	}
	return 0, nil
}
