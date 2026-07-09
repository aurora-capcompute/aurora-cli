package cli

import (
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
