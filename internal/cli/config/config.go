// Package config persists the terminal's working state — the mounted server
// and the current path in its virtual filesystem — the way a shell keeps a
// working directory, so `cd` once and every later command resolves relative
// to it. The store is a small JSON file resolved from $AURORA_CONFIG, else
// $XDG_CONFIG_HOME/aurora/context.json, else ~/.config/aurora/context.json.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Context is the saved working state. Commands default to these when their
// flags are unset; mount and cd update and persist them.
type Context struct {
	// Server is the mounted aurora-dist base URL.
	Server string `json:"server,omitempty"`
	// Path is the current directory in the distribution's virtual
	// filesystem; empty means the root.
	Path string `json:"path,omitempty"`
	// PrevPath backs `cd -`.
	PrevPath string `json:"prev_path,omitempty"`
}

// Path resolves the context file location.
func Path() (string, error) {
	if explicit := os.Getenv("AURORA_CONFIG"); explicit != "" {
		return explicit, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "aurora", "context.json"), nil
}

// Load reads the saved context, returning a zero Context when none exists.
func Load() (Context, error) {
	path, err := Path()
	if err != nil {
		return Context{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Context{}, nil
	}
	if err != nil {
		return Context{}, err
	}
	var ctx Context
	if err := json.Unmarshal(raw, &ctx); err != nil {
		return Context{}, err
	}
	return ctx, nil
}

// Save writes the context, creating the parent directory if needed.
func Save(ctx Context) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
