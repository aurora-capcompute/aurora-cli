package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandMentionsUnsetWorkdir(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "")
	in := "please read @file.txt now"
	if got := expandMentions(in); got != in {
		t.Fatalf("without $AURORA_WORKDIR the input must be unchanged: %q", got)
	}
}

func TestExpandMentionsRelativeBecomesMarkdownLink(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "/work/dir")
	cases := map[string]string{
		"read @file.txt":         "read [@file.txt](/work/dir/file.txt)",
		"@file.txt":              "[@file.txt](/work/dir/file.txt)",
		"open @sub/notes.md end": "open [@sub/notes.md](/work/dir/sub/notes.md) end",
		"@a.txt and @b.txt":      "[@a.txt](/work/dir/a.txt) and [@b.txt](/work/dir/b.txt)",
	}
	for in, want := range cases {
		if got := expandMentions(in); got != want {
			t.Fatalf("expandMentions(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandMentionsLeavesAbsoluteAndHome(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "/work/dir")
	for _, in := range []string{
		"read @/etc/hosts please",
		"check @~/notes.txt",
	} {
		if got := expandMentions(in); got != in {
			t.Fatalf("absolute/home mention must be left as-is: %q -> %q", in, got)
		}
	}
}

func TestExpandMentionsIgnoresEmail(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "/work/dir")
	in := "mail bob@example.com about the plan"
	if got := expandMentions(in); got != in {
		t.Fatalf("an email @ is not a mention: %q -> %q", in, got)
	}
}

func TestExpandMentionsIsIdempotent(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "/work/dir")
	// The @ inside a produced [@name](path) link follows a '[', not a word
	// boundary, so a second pass leaves it alone.
	once := expandMentions("read @file.txt")
	if twice := expandMentions(once); twice != once {
		t.Fatalf("expansion is not idempotent: %q -> %q", once, twice)
	}
}

func TestExpandMentionsWrapsTargetWithSpaces(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "/work/my dir")
	got := expandMentions("@file.txt")
	want := "[@file.txt](</work/my dir/file.txt>)"
	if got != want {
		t.Fatalf("a target with a space must be angle-wrapped: %q, want %q", got, want)
	}
}

func TestExpandMentionsCleansTraversal(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "/work/dir")
	// filepath.Join cleans the target; the label keeps the raw mention.
	if got := expandMentions("@sub/../f.txt"); got != "[@sub/../f.txt](/work/dir/f.txt)" {
		t.Fatalf("mention target should be cleaned: %q", got)
	}
}

func TestExpandMentionsAbsolutizesWorkdir(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "relative/work")
	got := expandMentions("@file.txt")
	open := strings.Index(got, "](")
	if open < 0 || !strings.HasSuffix(got, ")") {
		t.Fatalf("expected a markdown link: %q", got)
	}
	target := strings.TrimSuffix(got[open+2:], ")")
	target = strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
	if !filepath.IsAbs(target) {
		t.Fatalf("link target is not absolute: %q (from %q)", target, got)
	}
}

// --- @-completion ---

func suffixes(runes [][]rune) []string {
	out := make([]string, len(runes))
	for i, r := range runes {
		out[i] = string(r)
	}
	return out
}

func mentionWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"README.md", "main.go", "src/app.go", "src/serve.go", ".hidden"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AURORA_WORKDIR", dir)
	return dir
}

func TestCompleteMentionUnsetWorkdir(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "")
	if out, off := (&app{}).completeMention("@x"); out != nil || off != 0 {
		t.Fatalf("no workdir should offer nothing: %v %d", out, off)
	}
}

func TestCompleteMentionListsRoot(t *testing.T) {
	mentionWorkdir(t)
	out, off := (&app{}).completeMention("@")
	if off != 0 {
		t.Fatalf("offset = %d, want 0 for a bare @", off)
	}
	got := suffixes(out)
	want := []string{"README.md", "main.go", "src/"} // sorted; .hidden hidden; dir has slash
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("completions = %v, want %v", got, want)
	}
}

func TestCompleteMentionPrefix(t *testing.T) {
	mentionWorkdir(t)
	out, off := (&app{}).completeMention("@RE")
	if off != 2 {
		t.Fatalf("offset = %d, want 2 (len of RE)", off)
	}
	if got := suffixes(out); len(got) != 1 || got[0] != "ADME.md" {
		t.Fatalf("completions = %v, want [ADME.md]", got)
	}
}

func TestCompleteMentionDirectoryDrilldown(t *testing.T) {
	mentionWorkdir(t)
	out, off := (&app{}).completeMention("@src/")
	if off != 0 {
		t.Fatalf("offset = %d, want 0 after the slash", off)
	}
	got := suffixes(out)
	want := []string{"app.go", "serve.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("completions = %v, want %v", got, want)
	}

	out, off = (&app{}).completeMention("@src/se")
	if off != 2 || len(out) != 1 || string(out[0]) != "rve.go" {
		t.Fatalf("src/se completion = %v off=%d, want [rve.go] off=2", suffixes(out), off)
	}
}

func TestCompleteMentionDirectoryTrailingSlash(t *testing.T) {
	mentionWorkdir(t)
	out, _ := (&app{}).completeMention("@sr")
	if got := suffixes(out); len(got) != 1 || got[0] != "c/" {
		t.Fatalf("directory completion = %v, want [c/] (trailing slash)", got)
	}
}

func TestCompleteMentionDotfilesOnlyWhenTyped(t *testing.T) {
	mentionWorkdir(t)
	if out, _ := (&app{}).completeMention("@."); len(out) != 1 || string(out[0]) != "hidden" {
		t.Fatalf("typing @. should reveal dotfiles: %v", suffixes(out))
	}
}

func TestCompleteMentionSkipsAbsolute(t *testing.T) {
	mentionWorkdir(t)
	if out, _ := (&app{}).completeMention("@/etc"); out != nil {
		t.Fatalf("absolute mention should not complete against workdir: %v", suffixes(out))
	}
}

func TestCompleterRoutesSpawnMention(t *testing.T) {
	mentionWorkdir(t)
	c := completer{&app{}}
	line := []rune("spawn @RE")
	out, off := c.Do(line, len(line))
	if off != 2 || len(out) != 1 || string(out[0]) != "ADME.md" {
		t.Fatalf("completer.Do on a spawn @-mention = %v off=%d, want [ADME.md] off=2", suffixes(out), off)
	}
}
