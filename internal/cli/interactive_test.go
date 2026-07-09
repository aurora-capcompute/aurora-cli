package cli

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{`ls -l /ses_x`, []string{"ls", "-l", "/ses_x"}, false},
		{`spawn "summarize the news"`, []string{"spawn", "summarize the news"}, false},
		{`spawn 'do it now'`, []string{"spawn", "do it now"}, false},
		{`cd    proc_x`, []string{"cd", "proc_x"}, false},
		{`spawn "unterminated`, nil, true},
		{``, nil, false},
	}
	for _, tc := range cases {
		got, err := tokenize(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("tokenize(%q): expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("tokenize(%q): %v", tc.in, err)
			continue
		}
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("tokenize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCompleteCommands(t *testing.T) {
	c := completer{&app{}}

	// "sp" completes to "spawn": the remainder "awn", replacing 2 runes.
	got, length := c.Do([]rune("sp"), 2)
	if length != 2 || !reflect.DeepEqual(runeStrings(got), []string{"awn"}) {
		t.Fatalf("complete sp = %q len %d, want [awn] 2", runeStrings(got), length)
	}

	// "re" offers both retry and resolve.
	got, _ = c.Do([]rune("re"), 2)
	if strs := runeStrings(got); !has(strs, "try") || !has(strs, "solve") {
		t.Fatalf("complete re = %q, want retry+resolve", strs)
	}

	// An empty prefix offers every verb.
	got, _ = c.Do([]rune(""), 0)
	if len(got) != len(replCommands) {
		t.Fatalf("complete '' = %d candidates, want %d", len(got), len(replCommands))
	}
}

func TestPagerCommand(t *testing.T) {
	// Precedence: $AURORA_PAGER, then $PAGER, then the "less" default.
	t.Setenv("AURORA_PAGER", "")
	t.Setenv("PAGER", "")
	if got := pagerCommand(); got != "less" {
		t.Errorf("default pager = %q, want less", got)
	}
	t.Setenv("PAGER", "more")
	if got := pagerCommand(); got != "more" {
		t.Errorf("$PAGER pager = %q, want more", got)
	}
	t.Setenv("AURORA_PAGER", "bat -p")
	if got := pagerCommand(); got != "bat -p" {
		t.Errorf("$AURORA_PAGER pager = %q, want 'bat -p'", got)
	}
}

// When stdout isn't a terminal, page must not launch a pager — it prints the
// lines like cat, so `aurora less x | grep y` and non-interactive use work.
func TestPageFallsBackToPlainPrintWhenNotATTY(t *testing.T) {
	if isTerminal(os.Stdout) {
		t.Skip("stdout is a terminal; page would launch the pager")
	}
	var buf bytes.Buffer
	a := &app{out: &buf}
	if err := a.page([]string{"line one", "line two"}); err != nil {
		t.Fatalf("page: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Fatalf("page output = %q, want both lines", got)
	}
}

func runeStrings(rs [][]rune) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
