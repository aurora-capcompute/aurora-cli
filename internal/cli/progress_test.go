package cli

import (
	"encoding/json"
	"testing"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
)

// The agent's automatic save of each tool result is housekeeping, so a live
// follow does not show it. A note the model writes itself is a step, and shows.
func TestProgressHidesAutomaticSaves(t *testing.T) {
	save := client.JournalEntry{Syscall: client.Syscall{Name: "core.scratch", Args: json.RawMessage(`{"operation":"store","value":{}}`)}}
	if line := progressLine(save); line != "" {
		t.Fatalf("automatic save shown as %q", line)
	}
	note := client.JournalEntry{Syscall: client.Syscall{Name: "core.scratch", Args: json.RawMessage(`{"operation":"put","key":"k","value":"v"}`)}}
	if line := progressLine(note); line == "" {
		t.Fatal("a note the model writes is hidden")
	}
}
