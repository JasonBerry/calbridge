package main

import (
	"context"
	"io"
)

func runSpike(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return notImplemented("M2")(ctx, args, stdout, stderr)
}
