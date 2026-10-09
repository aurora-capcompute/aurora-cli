package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
)

// Revision 1 ran three calls. Revision 2 forked after the first one and ended
// after one call of its own. Each revision shows what it replayed: the newer
// one keeps the shared prefix and never shows the older one's tail.
func TestRevisionEntriesStopAtTheRevisionsEnd(t *testing.T) {
	process := client.ProcessLog{Process: client.Process{Revision: 2, JournalLength: 4}}
	for _, entry := range []struct {
		revision uint64
		position int
	}{{1, 0}, {1, 2}, {1, 4}, {2, 2}} {
		process.Entries = append(process.Entries, client.JournalEntry{Position: entry.position, Revision: entry.revision})
	}
	view := func(revision uint64) string {
		var out []string
		for _, entry := range revisionEntries(process, revision) {
			out = append(out, fmt.Sprintf("%d@%d", entry.Position, entry.Revision))
		}
		return strings.Join(out, " ")
	}
	if got := view(2); got != "0@1 2@2" {
		t.Fatalf("current revision = %q, want the shared prefix and its own call", got)
	}
	if got := view(1); got != "0@1 2@1 4@1" {
		t.Fatalf("older revision = %q, want all three of its calls", got)
	}
}
