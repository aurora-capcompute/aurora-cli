package config

import (
	"path/filepath"
	"testing"
)

func TestContextRoundTripsThroughAuroraConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctx.json")
	t.Setenv("AURORA_CONFIG", path)

	// A missing file loads as the zero context, not an error.
	loaded, err := Load()
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if loaded != (Context{}) {
		t.Fatalf("missing context = %+v, want zero", loaded)
	}

	want := Context{Server: "http://127.0.0.1:9090", Path: "/ses_1/proc_1", PrevPath: "/"}
	if err := Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got != want {
		t.Fatalf("reloaded = %+v, want %+v", got, want)
	}

	if resolved, err := Path(); err != nil || resolved != path {
		t.Fatalf("path = %q, %v; want %q", resolved, err, path)
	}
}

func TestPathHonorsXDG(t *testing.T) {
	t.Setenv("AURORA_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-home")
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join("/tmp/xdg-home", "aurora", "context.json") {
		t.Fatalf("path = %q", path)
	}
}
