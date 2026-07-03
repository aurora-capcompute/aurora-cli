// aurora-cli is the first Aurora terminal: a CLI binding directly to an
// aurora-dist /v1 API (trusted local single-principal use; no policy layer
// between). See `aurora-cli help`.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aurora-capcompute/aurora-cli/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := cli.Run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "aurora-cli:", err)
		os.Exit(1)
	}
}
