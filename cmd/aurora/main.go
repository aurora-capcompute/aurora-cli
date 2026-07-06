// aurora is the Aurora terminal: a shell over an aurora-dist /v1 API
// (trusted local single-principal use; no policy layer between). The
// distribution's state is a virtual filesystem; see `aurora help`.
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
		fmt.Fprintln(os.Stderr, "aurora:", err)
		os.Exit(1)
	}
}
