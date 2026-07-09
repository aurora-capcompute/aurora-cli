package cli

import (
	"path/filepath"
	"testing"
)

func TestExpandMentionsUnsetWorkdir(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "")
	in := "please read @file.txt now"
	if got := expandMentions(in); got != in {
		t.Fatalf("without $AURORA_WORKDIR the input must be unchanged: %q", got)
	}
}

func TestExpandMentionsRelative(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "/work/dir")
	cases := map[string]string{
		"read @file.txt":         "read @/work/dir/file.txt",
		"@file.txt":              "@/work/dir/file.txt",
		"open @sub/notes.md end": "open @/work/dir/sub/notes.md end",
		"@a.txt and @b.txt":      "@/work/dir/a.txt and @/work/dir/b.txt",
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

func TestExpandMentionsCleansTraversal(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "/work/dir")
	// filepath.Join cleans the path; the dispatcher's root check is the real
	// boundary, so the CLI need only produce a full path.
	if got := expandMentions("@sub/../f.txt"); got != "@/work/dir/f.txt" {
		t.Fatalf("mention path should be cleaned: %q", got)
	}
}

func TestExpandMentionsAbsolutizesWorkdir(t *testing.T) {
	t.Setenv("AURORA_WORKDIR", "relative/work")
	got := expandMentions("@file.txt")
	// The rewritten mention must carry an absolute path even when the working
	// directory was given relative.
	const prefix = "@"
	if len(got) < len(prefix) || got[:len(prefix)] != prefix {
		t.Fatalf("expected a mention: %q", got)
	}
	if !filepath.IsAbs(got[len(prefix):]) {
		t.Fatalf("mention path is not absolute: %q", got)
	}
}
