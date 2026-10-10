package cli

// The read commands: ls, cat, less, tail, stat and diff. They only read the
// tree, and every view is computed here from the one session read.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
)

func (a *app) ls(ctx context.Context, args []string) error {
	fs, server := a.flags("ls")
	long := fs.Bool("l", false, "one detailed line per entry")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	target := "."
	if len(rest) > 0 {
		target = rest[0]
	}
	n, err := a.resolveNode(ctx, target)
	if err != nil {
		return err
	}
	entries, err := a.list(ctx, n)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if *long {
			a.printf("%s", entry.long)
		} else {
			a.printf("%s", entry.name)
		}
	}
	return nil
}

func (a *app) cat(ctx context.Context, args []string) error {
	fs, server := a.flags("cat")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: cat <path>...")
	}
	lines, err := a.catAll(ctx, rest)
	if err != nil {
		return err
	}
	for _, line := range lines {
		a.printf("%s", line)
	}
	return nil
}

// catAll is the content of every target, in order.
func (a *app) catAll(ctx context.Context, targets []string) ([]string, error) {
	var lines []string
	for _, target := range targets {
		n, err := a.resolveNode(ctx, target)
		if err != nil {
			return nil, err
		}
		content, err := catLines(n)
		if err != nil {
			return nil, err
		}
		lines = append(lines, content...)
	}
	return lines, nil
}

// less shows a node's content through a pager instead of dumping it: same
// reads as cat (an entry, a manifest, history), but a long one scrolls in the
// pager rather than flooding the screen.
func (a *app) less(ctx context.Context, args []string) error {
	fs, server := a.flags("less")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: less <path>...")
	}
	lines, err := a.catAll(ctx, rest)
	if err != nil {
		return err
	}
	return a.page(lines)
}

// page views lines through the user's pager — $AURORA_PAGER, else $PAGER, else
// `less`. The content is handed over as a temp file passed as the pager's
// argument, so any pager or editor works uniformly (less, bat, vim, nano). When
// stdout is not a terminal (piped or redirected) paging is pointless, so the
// lines are written straight out as cat would.
func (a *app) page(lines []string) error {
	if !isTerminal(os.Stdout) {
		for _, line := range lines {
			a.printf("%s", line)
		}
		return nil
	}
	tmp, err := os.CreateTemp("", "aurora-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// A shell runs the command so $PAGER may carry flags ("less -R"); the temp
	// path rides in as a positional ("$@"), never interpolated, so a path with
	// spaces or shell metacharacters can't break the command.
	cmd := exec.Command("sh", "-c", pagerCommand()+` "$@"`, "aurora-less", tmp.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// pagerCommand resolves the pager: $AURORA_PAGER, else $PAGER, else `less`.
func pagerCommand() string {
	return cmp.Or(strings.TrimSpace(os.Getenv("AURORA_PAGER")), strings.TrimSpace(os.Getenv("PAGER")), "less")
}

// isTerminal reports whether f is a character device — a real terminal — as
// opposed to a pipe or a regular file, so paging is only attempted when it can
// actually be driven.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// tail prints the last N entries of a directory — the most recent processes
// of a session, the newest journal entries of a process, the latest sessions
// at the root — or the last N lines of a file.
func (a *app) tail(ctx context.Context, args []string) error {
	fs, server := a.flags("tail")
	count := fs.Int("n", 10, "entries to show")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	target := "."
	if len(rest) > 0 {
		target = rest[0]
	}
	n, err := a.resolveNode(ctx, target)
	if err != nil {
		return err
	}
	var lines []string
	if n.isDir() {
		entries, err := a.list(ctx, n)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			lines = append(lines, entry.long)
		}
	} else if lines, err = catLines(n); err != nil {
		return err
	}
	if *count > 0 && len(lines) > *count {
		lines = lines[len(lines)-*count:]
	}
	for _, line := range lines {
		a.printf("%s", line)
	}
	return nil
}

func (a *app) stat(ctx context.Context, args []string) error {
	fs, server := a.flags("stat")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: stat <path>")
	}
	n, err := a.resolveNode(ctx, rest[0])
	if err != nil {
		return err
	}
	switch n.kind {
	case nodeRoot:
		summaries, err := a.client.ListSessions(ctx)
		if err != nil {
			return err
		}
		return a.emitJSON(map[string]any{"server": a.client.BaseURL, "sessions": len(summaries)})
	case nodePrograms:
		programs, err := a.client.Programs(ctx)
		if err != nil {
			return err
		}
		return a.emitJSON(map[string]any{"programs": len(programs)})
	case nodeProgram:
		return a.emitJSON(n.program)
	case nodeSession:
		return a.emitJSON(n.log.Session)
	case nodeHistory:
		return a.emitJSON(map[string]any{"turns": len(n.log.History)})
	case nodeProcess:
		return a.emitJSON(struct {
			client.Process
			Entries int `json:"entries"`
			Tasks   int `json:"tasks"`
		}{n.process.Process,
			len(revisionEntries(n.process, n.process.Revision)), len(n.process.Tasks)})
	case nodeProcessFile:
		lines, err := catLines(n)
		if err != nil {
			return err
		}
		return a.emitJSON(map[string]any{"file": n.file, "lines": len(lines)})
	case nodeEntry:
		return a.emitJSON(n.entry)
	case nodeRevisions:
		return a.emitJSON(map[string]any{"revisions": n.process.Revision})
	case nodeRevision:
		return a.emitJSON(map[string]any{
			"revision": n.revision,
			"entries":  len(revisionEntries(n.process, n.revision)),
		})
	case nodeTasks:
		return a.emitJSON(map[string]any{"tasks": len(n.process.Tasks)})
	case nodeTask:
		return a.emitJSON(n.task)
	}
	return noEnt(n.path)
}

// diff shows where two revisions of one process diverge: the shared prefix
// both replayed, then the rolled-back entries (-) and their re-execution (+).
func (a *app) diff(ctx context.Context, args []string) error {
	fs, server := a.flags("diff")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return errors.New("usage: diff <process|process/revisions/N> <process|process/revisions/M>")
	}
	left, err := a.revisionView(ctx, rest[0])
	if err != nil {
		return err
	}
	right, err := a.revisionView(ctx, rest[1])
	if err != nil {
		return err
	}
	if left.process.ID != right.process.ID {
		return errors.New("diff compares two revisions of one process")
	}
	before := revisionEntries(left.process, left.revision)
	after := revisionEntries(right.process, right.revision)
	shared := 0
	for shared < len(before) && shared < len(after) &&
		before[shared].Position == after[shared].Position &&
		before[shared].Revision == after[shared].Revision {
		shared++
	}
	if shared == len(before) && shared == len(after) {
		a.printf("identical (%d entries)", shared)
		return nil
	}
	if shared > 0 {
		a.printf("shared prefix: #%d..#%d (%d entries)", before[0].Position, before[shared-1].Position, shared)
	}
	for _, entry := range before[shared:] {
		a.printf("- %s", renderEntry(entry, 96))
	}
	for _, entry := range after[shared:] {
		a.printf("+ %s", renderEntry(entry, 96))
	}
	return nil
}

// revisionView resolves a diff argument: a revision directory, or a process
// (meaning its current revision).
func (a *app) revisionView(ctx context.Context, arg string) (node, error) {
	n, err := a.resolveNode(ctx, arg)
	if err != nil {
		return node{}, err
	}
	switch n.kind {
	case nodeRevision:
		return n, nil
	case nodeProcess:
		n.revision = n.process.Revision
		return n, nil
	}
	return node{}, fmt.Errorf("%s: not a process or revision", n.path)
}

// revisionEntries is the journal one revision of a process replays: for each
// position, the entry with the highest revision ≤ revision (copy-on-write forks
// leave earlier-revision entries in the shared prefix), up to the revision's
// end. An earlier revision may have run past that end. The current revision
// ends at the process's journal length. An older revision ends at the last entry
// it wrote itself, so one that wrote nothing shows no entries.
func revisionEntries(process client.ProcessLog, revision uint64) []client.JournalEntry {
	end := process.JournalLength
	if revision != process.Revision {
		end = 0
		for _, entry := range process.Entries {
			if entry.Revision == revision {
				end = max(end, entry.Position+1)
			}
		}
	}
	best := map[int]client.JournalEntry{}
	for _, entry := range process.Entries {
		if entry.Revision > revision || entry.Position >= end {
			continue
		}
		if current, ok := best[entry.Position]; !ok || entry.Revision > current.Revision {
			best[entry.Position] = entry
		}
	}
	out := make([]client.JournalEntry, 0, len(best))
	for _, position := range slices.Sorted(maps.Keys(best)) {
		out = append(out, best[position])
	}
	return out
}
