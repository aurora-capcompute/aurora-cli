package cli

// Rendering for the terminal. Guest and model text can carry terminal escape
// sequences, so every such string passes sanitizeTerminal before it is
// printed.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
)

func processLine(process client.Process) string {
	extra := ""
	if process.Answer != "" {
		extra = "  " + truncate(sanitizeTerminal(process.Answer), 60)
	}
	if process.Error != "" {
		extra = "  ! " + truncate(sanitizeTerminal(process.Error), 60)
	}
	return fmt.Sprintf("%s  %-16s %s%s", process.ID, process.Status, truncate(sanitizeTerminal(process.Input), 48), extra)
}

func renderEntry(entry client.JournalEntry, limit int) string {
	line := fmt.Sprintf("#%-3d r%-2d %-14s", entry.Position, entry.Revision, entry.Syscall.Name)
	if len(entry.Syscall.Args) > 0 {
		line += " " + compact(entry.Syscall.Args, limit)
	}
	switch entry.Outcome.Status {
	case "result":
		line += " → " + compact(entry.Outcome.Result, limit)
	case "failed":
		line += fmt.Sprintf(" ✗ %s: %s", entry.Outcome.Code, sanitizeTerminal(entry.Outcome.Message))
	case "yield":
		line += " ⏳ " + sanitizeTerminal(entry.Outcome.Message)
	}
	if len(entry.Outcome.Labels) > 0 {
		line += "  [" + sanitizeTerminal(strings.Join(entry.Outcome.Labels, " ")) + "]"
	}
	return line
}

// taskLine renders a task the way ls -l renders a symlink: a durable task's
// identity is the open intent at its journal position, so show the link.
func taskLine(task client.Task) string {
	line := fmt.Sprintf("%s -> ../%d  %-9s %-14s %s",
		task.ID, task.JournalPosition, task.State, task.Syscall.Name, sanitizeTerminal(truncate(task.Summary, 48)))
	if task.Resolution.Decision != "" {
		line += fmt.Sprintf("  (resolved %s by %s)", task.Resolution.Decision, task.Resolution.Actor)
	}
	return line
}

// pendingCall is the call a task holds, as a human deciding on it should see
// it: the syscall and its complete arguments, never a summary.
func pendingCall(task client.Task) string {
	return strings.TrimSpace(task.Syscall.Name + " " + compact(task.Syscall.Args, 0))
}

func processFileLong(proc client.ProcessLog, file string) string {
	switch file {
	case "status":
		return fmt.Sprintf("%-9s %s (attempt %d, revision %d)", file, proc.Status, proc.Attempt, proc.Revision)
	case "input":
		return fmt.Sprintf("%-9s %s", file, truncate(proc.Input, 72))
	case "answer":
		return fmt.Sprintf("%-9s %s", file, truncate(proc.Answer, 72))
	case "error":
		return fmt.Sprintf("%-9s %s", file, truncate(proc.Error, 72))
	case "manifest":
		return fmt.Sprintf("%-9s %s", file, compact(proc.Manifest, 72))
	}
	return file
}

// fileLong is a file node's -l line.
func fileLong(n node) string {
	switch n.kind {
	case nodeHistory:
		return fmt.Sprintf("history  %d turns", len(n.log.History))
	case nodeProcessFile:
		return processFileLong(n.process, n.file)
	case nodeEntry:
		return renderEntry(n.entry, 96)
	case nodeTask:
		return taskLine(n.task)
	case nodeProgram:
		return fmt.Sprintf("%-20s %s  %s", n.program.ID, truncate(n.program.Digest, 16), sanitizeTerminal(truncate(n.program.Description, 56)))
	}
	return path.Base(n.path)
}

func valueLines(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(sanitizeTerminal(value), "\n")
}

func jsonLines(value any) ([]string, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return strings.Split(string(raw), "\n"), nil
}

// compact renders raw JSON on one line, truncated to limit runes (0 = no
// truncation). The output is terminal-sanitized: journal args and results are
// guest-authored, and a lifecycle syscall published with no InputSchema (e.g.
// sys.output) journals its args unvalidated — so a guest can land raw,
// non-JSON bytes as an intent. json.Compact then fails and the fallback writes
// those bytes verbatim. sanitizeTerminal neutralizes any escape sequences
// before they reach the operator's terminal. On valid JSON the control
// characters are already \u-escaped, so this changes nothing.
func compact(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	return truncate(sanitizeTerminal(strings.ReplaceAll(buf.String(), "\n", " ")), limit)
}

func truncate(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// quoteTitle quotes a title for display, keeping it on one line. The title is
// derived from guest/model-authored input, so escapes are neutralized before it
// reaches the terminal.
func quoteTitle(title string) string {
	if title == "" {
		return ""
	}
	return "\"" + strings.ReplaceAll(sanitizeTerminal(title), "\n", " ") + "\""
}

// sanitizeTerminal neutralizes control characters in guest- or model-authored
// text before it reaches the operator's terminal. The CLI is the operator's
// audit lens over untrusted guest activity, so an answer, error, fetched web
// page, or journal message that smuggles ANSI/OSC escape sequences (or a
// carriage return / backspace) could otherwise repaint or hide what a process
// actually did. Newline and tab are preserved for layout; every other C0/C1
// control (and DEL) becomes the visible, inert replacement rune.
func sanitizeTerminal(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return '�'
		}
		return r
	}, s)
}
