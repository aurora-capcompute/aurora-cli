package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
	"github.com/aurora-capcompute/aurora-cli/internal/cli/config"
)

// memoryStub serves the two read-only /v1/memory endpoints over a fixed key
// set, mirroring the server's list semantics (a prefix matches the exact key
// or anything under it).
func memoryStub(t *testing.T) (*httptest.Server, map[string]client.MemoryValue) {
	t.Helper()
	values := map[string]client.MemoryValue{
		"s/ses_1/prefs/tone": {
			Key: "s/ses_1/prefs/tone", Found: true,
			Value: json.RawMessage(`"formal"`), Version: 1,
		},
		"shared/team-kb/handoff": {
			Key: "shared/team-kb/handoff", Found: true,
			Value: json.RawMessage(`{"owner":"alice"}`), Version: 2,
		},
		// A value that smuggles a live terminal escape — the poisoning case the
		// view must neutralize — written by a run that had observed the web.
		"shared/team-kb/notes/today": {
			Key: "shared/team-kb/notes/today", Found: true,
			Value: json.RawMessage(`"before\u001b[2J\rafter"`), Version: 3,
			Labels: []string{"untrusted_web"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/memory", func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("prefix")
		keys := []client.MemoryEntry{}
		for key, value := range values {
			if prefix == "" || key == prefix || strings.HasPrefix(key, prefix+"/") {
				keys = append(keys, client.MemoryEntry{Key: key, Labels: value.Labels})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	mux.HandleFunc("GET /v1/memory/value", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		value, ok := values[key]
		if !ok {
			value = client.MemoryValue{Key: key}
		}
		_ = json.NewEncoder(w).Encode(value)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, values
}

func memoryApp(t *testing.T) *app {
	t.Helper()
	server, _ := memoryStub(t)
	return &app{ctx: config.Context{}, out: &strings.Builder{}, client: client.New(server.URL)}
}

// /memory browses like a directory: the root shows the scope prefixes, a
// deeper path shows its immediate children with subdirectories marked, and a
// listed key carries its provenance labels so a tainted value is flagged
// before it is even opened.
func TestMemoryTreeListsAsDirectories(t *testing.T) {
	a := memoryApp(t)
	ctx := context.Background()

	names := func(p string) []string {
		t.Helper()
		n, err := a.resolveNode(ctx, p)
		if err != nil {
			t.Fatalf("resolve %s: %v", p, err)
		}
		if !n.isDir() {
			t.Fatalf("%s did not resolve to a directory", p)
		}
		entries, err := a.list(ctx, n)
		if err != nil {
			t.Fatalf("list %s: %v", p, err)
		}
		out := make([]string, 0, len(entries))
		for _, entry := range entries {
			out = append(out, entry.name)
		}
		return out
	}

	if got := names("/memory"); len(got) != 2 || got[0] != "s/" || got[1] != "shared/" {
		t.Fatalf("ls /memory = %v, want [s/ shared/]", got)
	}
	if got := names("/memory/shared/team-kb"); len(got) != 2 || got[0] != "handoff" || got[1] != "notes/" {
		t.Fatalf("ls /memory/shared/team-kb = %v, want [handoff notes/]", got)
	}

	// The tainted key's -l line shows its provenance labels.
	n, err := a.resolveNode(ctx, "/memory/shared/team-kb/notes")
	if err != nil {
		t.Fatalf("resolve notes: %v", err)
	}
	entries, err := a.list(ctx, n)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list notes = %v, %v", entries, err)
	}
	if !strings.Contains(entries[0].long, "untrusted_web") {
		t.Fatalf("listing hides the value's provenance: %q", entries[0].long)
	}
}

// cat of a stored value decodes a JSON string to its text and neutralizes
// control characters — the value is agent-written (here: tainted by the web),
// and the terminal must not execute what an attacker stored.
func TestMemoryCatSanitizesStoredValue(t *testing.T) {
	a := memoryApp(t)
	n, err := a.resolveNode(context.Background(), "/memory/shared/team-kb/notes/today")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.kind != nodeMemoryKey || n.isDir() {
		t.Fatalf("key resolved to kind %v", n.kind)
	}
	lines, err := catLines(n)
	if err != nil {
		t.Fatalf("cat: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.ContainsRune(joined, 0x1b) || strings.ContainsRune(joined, '\r') {
		t.Fatalf("cat leaked a terminal control character: %q", joined)
	}
	if !strings.Contains(joined, "before") || !strings.Contains(joined, "after") {
		t.Fatalf("cat lost the value's text: %q", joined)
	}

	// A JSON object pretty-prints; its own escaping keeps it inert.
	obj, err := a.resolveNode(context.Background(), "/memory/shared/team-kb/handoff")
	if err != nil {
		t.Fatalf("resolve handoff: %v", err)
	}
	lines, err = catLines(obj)
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), `"owner": "alice"`) {
		t.Fatalf("cat handoff = %v, %v", lines, err)
	}

	// An absent path is a plain no-such-file, and the memory root resolves even
	// when a segment matches nothing else.
	if _, err := a.resolveNode(context.Background(), "/memory/shared/absent"); err == nil {
		t.Fatal("absent memory path resolved")
	}
}
