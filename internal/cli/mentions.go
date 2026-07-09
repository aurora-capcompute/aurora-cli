package cli

// @-mention resolution. In a spawn input a file is referenced by a short
// mention — `@file.txt` — and the terminal rewrites it to its full path under a
// saved working directory, so `@file.txt` becomes `@/work/dir/file.txt`. The
// working directory is $AURORA_WORKDIR (like $AURORA_MANIFEST states the grants
// once); an agent granted a filesystem capability rooted there then reads the
// resolved path.
//
// In the interactive shell, `@` also drives completion: typing `@` and a
// partial name lists the matching files under $AURORA_WORKDIR (see
// completeMention), so a mention is discovered by search and finished by the
// same Tab that completes every other path. The full path is substituted when
// the line is submitted — a terminal completion can only append to the typed
// word, not rewrite it — so the two halves meet at spawn: pick the file, run
// the line.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// mentionPattern matches an @-mention at a word boundary: an `@` at the start of
// the input or just after whitespace, followed by a path token. The boundary
// anchor keeps it from firing on the `@` inside an email address or handle
// (`user@host`), where the `@` is preceded by a non-space character.
var mentionPattern = regexp.MustCompile(`(^|\s)@([A-Za-z0-9._~/\-]+)`)

// workingDir returns the absolute $AURORA_WORKDIR and whether it is set. It is
// the shared base for both resolving and completing mentions.
func workingDir() (string, bool) {
	dir := strings.TrimSpace(os.Getenv("AURORA_WORKDIR"))
	if dir == "" {
		return "", false
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir, true
}

// expandMentions rewrites relative @-mentions in a spawn input to their full
// path under $AURORA_WORKDIR: `@file.txt` becomes `@/work/dir/file.txt`. When
// $AURORA_WORKDIR is unset the input is returned unchanged. A mention that is
// already absolute (`@/etc/hosts`) or home-anchored (`@~/x`) is left exactly as
// written — only relative mentions are resolved against the working directory.
func expandMentions(input string) string {
	dir, ok := workingDir()
	if !ok {
		return input
	}
	return mentionPattern.ReplaceAllStringFunc(input, func(match string) string {
		// The match holds the leading boundary (start-of-string, so "", or the
		// single whitespace rune) plus the @path; keep the boundary verbatim.
		at := strings.IndexByte(match, '@')
		lead, mention := match[:at], match[at+1:]
		if mention == "" || strings.HasPrefix(mention, "/") || strings.HasPrefix(mention, "~") {
			return match
		}
		return lead + "@" + filepath.ToSlash(filepath.Join(dir, mention))
	})
}

// completeMentionLimit caps how many files one @-completion offers, so a large
// directory does not flood the menu.
const completeMentionLimit = 200

// completeMention completes an @-mention against the files under $AURORA_WORKDIR:
// given the word `@src/ma` it lists `src/` and returns the entries whose name
// begins with `ma`, directories carrying a trailing slash so completion drills
// into them. It returns the readline completion pair (suffixes to append, and
// the length of the shared prefix). Completion is prefix-based and
// case-sensitive because a terminal completion can only append to the typed
// text, never rewrite its case. With $AURORA_WORKDIR unset, or for an absolute
// or home-anchored mention, it offers nothing.
func (a *app) completeMention(word string) ([][]rune, int) {
	dir, ok := workingDir()
	if !ok {
		return nil, 0
	}
	query := strings.TrimPrefix(word, "@")
	if strings.HasPrefix(query, "/") || strings.HasPrefix(query, "~") {
		return nil, 0
	}
	sub, base := "", query
	if i := strings.LastIndex(query, "/"); i >= 0 {
		sub, base = query[:i+1], query[i+1:]
	}
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(sub)))
	if err != nil {
		return nil, len([]rune(base))
	}
	var out [][]rune
	for _, entry := range entries {
		name := entry.Name()
		// Hide dotfiles unless the user is explicitly typing one.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if !strings.HasPrefix(name, base) {
			continue
		}
		suffix := name[len(base):]
		if entry.IsDir() {
			suffix += "/"
		}
		out = append(out, []rune(suffix))
		if len(out) >= completeMentionLimit {
			break
		}
	}
	return out, len([]rune(base))
}
