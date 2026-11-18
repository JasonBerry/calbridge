package main

import (
	"context"
	"errors"
	"io"

	"github.com/JasonBerry/calbridge/internal/spike"
)

func runSpike(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	err := spike.Main(ctx, args, stdout, stderr)
	if errors.Is(err, spike.ErrUsage) {
		return errUsage
	}
	return err
}
