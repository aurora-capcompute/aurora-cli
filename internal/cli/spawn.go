package cli

// spawn: run a process and follow it to its answer, showing its steps as
// they are journaled.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
)

func (a *app) spawn(ctx context.Context, args []string) error {
	fs, server := a.flags("spawn")
	manifestPath := fs.String("manifest", "", "path to a manifest JSON file (- reads stdin); defaults to $AURORA_MANIFEST")
	detach := fs.Bool("detach", false, "print the process id and exit instead of following")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: spawn <input> [-manifest file.json|-] [-detach]")
	}
	input := strings.Join(rest, " ")

	// The manifest is an explicit input, never inherited from the session: the
	// -manifest flag, else $AURORA_MANIFEST. The server refuses a process
	// without one, because the manifest names the program.
	manifest, err := readManifest(*manifestPath)
	if err != nil {
		return err
	}
	sessionID, err := a.cwdSession()
	if err != nil {
		return err
	}
	process, err := a.client.CreateProcess(ctx, sessionID, input, manifest)
	if err != nil {
		return err
	}
	a.printf("process %s", process.ID)
	if *detach {
		return nil
	}
	return a.pollToAnswer(ctx, process.ID)
}

// readManifest loads the manifest: the -manifest argument (a file path, or - for
// stdin), else $AURORA_MANIFEST (a file path). Empty everywhere sends no
// manifest, and the server refuses the process. The manifest is never inherited
// from the session; it is an explicit input the caller sets, like an env var.
func readManifest(path string) (json.RawMessage, error) {
	if path == "" {
		path = strings.TrimSpace(os.Getenv("AURORA_MANIFEST"))
	}
	switch path {
	case "":
		return nil, nil
	case "-":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read manifest from stdin: %w", err)
		}
		return raw, nil
	default:
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		return raw, nil
	}
}

// pollToAnswer polls the process's status until it reaches a terminal state,
// printing the answer (or the failure). A process that parks on a task — an
// approval or a timer — keeps being polled: a timer resolves itself and an
// approval is resolved out-of-band, so a hint is printed once and following
// continues until the process finishes.
func (a *app) pollToAnswer(ctx context.Context, processID string) error {
	hinted := false
	lastLen, shown := 0, 0
	for {
		process, err := a.client.GetProcess(ctx, processID)
		if err != nil {
			return err
		}
		// Surface live progress: when the journal grows, print the new syscalls
		// the process has run so the wait shows what it is doing, not a blank
		// screen. JournalLength (cheap) gates the fuller session read.
		if process.JournalLength > lastLen {
			lastLen = process.JournalLength
			shown = a.followProgress(ctx, process.SessionID, processID, shown)
		}
		switch {
		case process.Status == "completed":
			a.printf("✔ %s", sanitizeTerminal(process.Answer))
			return nil
		case process.Status == "failed":
			return fmt.Errorf("process failed: %s", sanitizeTerminal(process.Error))
		case process.Status == "stopped":
			a.printf("■ stopped")
			return nil
		case process.Status == "interrupted":
			return fmt.Errorf("process interrupted: %s", sanitizeTerminal(process.Error))
		case process.Parked() && !hinted:
			a.printf("⏸ %s%s; still following", process.Status, a.parkHint(ctx, process))
			hinted = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// followProgress prints the process's new journal entries (those past `shown`)
// as one-line "what it's doing" summaries and returns the new high-water mark.
// Best-effort: a failed read just skips this round.
func (a *app) followProgress(ctx context.Context, sessionID, processID string, shown int) int {
	log, err := a.client.Session(ctx, sessionID)
	if err != nil {
		return shown
	}
	for _, p := range log.Processes {
		if p.ID != processID {
			continue
		}
		entries := revisionEntries(p, p.Revision)
		for _, entry := range entries[min(shown, len(entries)):] {
			if line := progressLine(entry); line != "" {
				a.printf("%s", line)
			}
		}
		return len(entries)
	}
	return shown
}

// progressLine is a compact "what it's doing" summary of one journal entry — the
// syscall and a short hint — or "" for the runtime's own plumbing, which is
// noise during a follow.
func progressLine(entry client.JournalEntry) string {
	if isProgressNoise(entry.Syscall.Name) || isAutoSave(entry.Syscall) {
		return ""
	}
	line := "  · " + entry.Syscall.Name
	if hint := syscallHint(entry.Syscall.Name, entry.Syscall.Args); hint != "" {
		line += " " + hint
	}
	switch entry.Outcome.Status {
	case "failed":
		line += " ✗ " + entry.Outcome.Code
	case "yield":
		line += " ⏳"
	}
	// The hint carries guest/model-authored syscall args (url, key, label);
	// neutralize any terminal escapes before this reaches a live follow.
	return sanitizeTerminal(line)
}

// isProgressNoise reports whether a syscall is runtime plumbing not worth
// showing in a live follow (the I/O envelope, savepoints, journaled world reads).
func isProgressNoise(name string) bool {
	switch name {
	case "sys.begin", "sys.commit", "sys.input", "sys.output", "sys.now", "sys.random", "sys.log":
		return true
	}
	return false
}

// isAutoSave reports whether a call is the agent's automatic save of a tool
// result (core.scratch store), which is housekeeping, not a step of the work.
func isAutoSave(call client.Syscall) bool {
	var args struct {
		Operation string `json:"operation"`
	}
	return call.Name == "core.scratch" && json.Unmarshal(call.Args, &args) == nil && args.Operation == "store"
}

// syscallHint pulls a short, human-meaningful detail from a syscall's args (the
// operation, the URL, the key) for the progress line; "" when there is none.
func syscallHint(name string, args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	switch name {
	case "core.openaiApi":
		return str("operation")
	case "core.internet":
		return strings.TrimSpace(str("method") + " " + hostPath(str("url")))
	case "sys.timer":
		if label := str("label"); label != "" {
			return label
		}
		return "timer"
	}
	return ""
}

// hostPath strips a URL's scheme for a compact progress hint.
func hostPath(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	return truncate(s, 60)
}

// parkHint says what a parked process actually waits on: a timer (it fires
// on its own — a nap, or an abort-retry) needs nothing, anything else needs
// a human resolution.
func (a *app) parkHint(ctx context.Context, process client.Process) string {
	log, err := a.client.Session(ctx, process.SessionID)
	if err != nil {
		return ""
	}
	for _, candidate := range log.Processes {
		if candidate.ID != process.ID {
			continue
		}
		for _, task := range candidate.Tasks {
			if task.State != "pending" {
				continue
			}
			if task.Syscall.Name != "sys.timer" {
				return fmt.Sprintf(" — task %s awaits resolution (`approve %s` or `deny`):\n    %s",
					task.ID, task.ID, pendingCall(task))
			}
		}
		return " — waiting on a timer; it fires on its own"
	}
	return ""
}
