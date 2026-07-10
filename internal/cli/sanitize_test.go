package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
)

// The CLI is the operator's audit lens over untrusted guest activity, so
// control characters in guest- or model-authored text must be neutralized
// before they reach the terminal — an escape sequence could otherwise repaint
// or hide what a process did.
func TestSanitizeTerminalNeutralizesControlSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ansi color", "safe\x1b[31mred\x1b[0m", "safe�[31mred�[0m"},
		{"osc title spoof", "ok\x1b]0;pwned\x07", "ok�]0;pwned�"},
		{"carriage return overwrite", "real\rfake", "real�fake"},
		{"backspace erase", "abc\x08\x08", "abc��"},
		{"del", "x\x7fy", "x�y"},
		{"c1 control", "x\x9by", "x�y"},
		{"newline and tab preserved", "line1\nline2\tcol", "line1\nline2\tcol"},
		{"ordinary text untouched", "hello, wörld — 42", "hello, wörld — 42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTerminal(tc.in); got != tc.want {
				t.Fatalf("sanitizeTerminal(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A guest answer/input carrying an escape sequence is neutralized in the process
// line an operator sees in `ls`/`tree`.
func TestProcessLineSanitizesGuestFields(t *testing.T) {
	line := processLine(client.Process{
		ID:     "proc_1",
		Status: "completed",
		Input:  "do\x1b]0;spoofed\x07 it",
		Answer: "done\x1b[2K\rHACKED",
	})
	if strings.ContainsRune(line, 0x1b) || strings.ContainsRune(line, '\r') {
		t.Fatalf("processLine leaked a terminal control character: %q", line)
	}
}

// A process answer becomes an assistant turn in session history, so `cat
// <session>/history` renders guest-authored content. It must be sanitized just
// like the sibling <session>/<proc>/answer view (valueLines) — otherwise the
// same string is safe in one lens and a raw escape sink in the other.
func TestCatHistorySanitizesGuestContent(t *testing.T) {
	n := node{
		kind: nodeHistory,
		path: "/s/history",
		log: client.SessionLog{History: []client.Message{
			{Role: "user", Content: "what's the status?"},
			{Role: "assistant", Content: "all\x1b[2J\x1b[31mSESSION CLEAN\x1b[0m\rHACKED"},
		}},
	}
	lines, err := catLines(n)
	if err != nil {
		t.Fatalf("catLines: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.ContainsRune(joined, 0x1b) || strings.ContainsRune(joined, '\r') {
		t.Fatalf("cat of history leaked a terminal control character: %q", joined)
	}
	if !strings.Contains(joined, "SESSION CLEAN") {
		t.Fatalf("sanitizing must keep the visible text, only neutralize controls: %q", joined)
	}
}

// A lifecycle syscall published with no InputSchema (e.g. sys.output) journals
// its args unvalidated, so a guest can land raw, non-JSON bytes as an intent.
// Rendering that journal entry (ls/cat of a revision) must not write those bytes
// to the terminal: json.Compact fails on them and the fallback would emit them
// verbatim without the sanitize guard.
func TestRenderEntrySanitizesRawJournalBytes(t *testing.T) {
	entry := client.JournalEntry{
		Position: 3,
		Syscall: client.Syscall{
			Name: "sys.output",
			// Not valid JSON: forces compact's raw-bytes fallback.
			Args: json.RawMessage("\x1b[2J\x1b]0;pwned\x07raw"),
		},
		Outcome: client.Outcome{
			Status: "result",
			Result: json.RawMessage("\x1b[31mraw-result\r"),
		},
	}
	line := renderEntry(entry, 0)
	if strings.ContainsRune(line, 0x1b) || strings.ContainsRune(line, '\r') || strings.ContainsRune(line, 0x07) {
		t.Fatalf("renderEntry leaked a terminal control character: %q", line)
	}
}

// Valid JSON args/results still render intact — the sanitize guard is a no-op on
// them because json.Marshal already escapes control characters to \uXXXX.
func TestRenderEntryKeepsValidJSONIntact(t *testing.T) {
	entry := client.JournalEntry{
		Position: 1,
		Syscall: client.Syscall{
			Name: "core.memory",
			Args: json.RawMessage(`{"operation":"put","key":"notes/a"}`),
		},
		Outcome: client.Outcome{Status: "result", Result: json.RawMessage(`{"version":2}`)},
	}
	line := renderEntry(entry, 0)
	if !strings.Contains(line, `"operation":"put"`) || !strings.Contains(line, `"version":2`) {
		t.Fatalf("valid JSON was mangled: %q", line)
	}
}
