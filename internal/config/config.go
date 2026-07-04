// Package config persists the terminal's working context — the current
// server, session, and process — the way kubectl persists its current
// context, so a session or process chosen once need not be retyped on every
// command. The store is a small JSON file resolved from $AURORA_CONFIG, else
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
// -server/-s/-p flags are unset; use/new/send update and persist them.
type Context struct {
	Server  string `json:"server,omitempty"`
	Session string `json:"session,omitempty"`
	Process string `json:"process,omitempty"`
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
