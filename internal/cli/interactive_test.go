package cli

import (
	"reflect"
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
