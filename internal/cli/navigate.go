package cli

// Moving around the tree: where am I, change directory, change server.

import (
	"context"
	"strings"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/config"
)

// whereami is the bare `aurora` invocation: the mount and the cwd.
func (a *app) whereami(context.Context) error {
	a.connect("")
	a.printf("%s on / type aurora-dist", a.client.BaseURL)
	a.printf("%s", a.cwd())
	return nil
}

func (a *app) pwd(_ context.Context, args []string) error {
	fs, server := a.flags("pwd")
	if _, err := a.bind(fs, server, args); err != nil {
		return err
	}
	a.printf("%s", a.cwd())
	return nil
}

func (a *app) cd(ctx context.Context, args []string) error {
	fs, server := a.flags("cd")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	target := "/"
	if len(rest) > 0 {
		target = rest[0]
	}
	if target == "-" {
		target = a.ctx.PrevPath
		if target == "" {
			target = "/"
		}
	}
	n, err := a.resolveNode(ctx, target)
	if err != nil {
		return err
	}
	if !n.isDir() {
		return notDir(n.path)
	}
	a.ctx.PrevPath = a.cwd()
	a.ctx.Path = n.path
	return config.Save(a.ctx)
}

func (a *app) mount(_ context.Context, args []string) error {
	fs, server := a.flags("mount")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		a.printf("%s on / type aurora-dist", a.client.BaseURL)
		return nil
	}
	// A different server is a different filesystem: reset the cwd with it.
	a.ctx.Server = strings.TrimRight(rest[0], "/")
	a.ctx.Path = ""
	a.ctx.PrevPath = ""
	return config.Save(a.ctx)
}
