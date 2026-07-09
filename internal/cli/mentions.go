package cli

// @-mention resolution. A spawn input may reference a file by a short mention —
// `@file.txt` — and the terminal rewrites it to carry the full path under a
// saved working directory, so `@file.txt` becomes `@/work/dir/file.txt`. The
// working directory is $AURORA_WORKDIR (like $AURORA_MANIFEST states the grants
// once); an agent granted a filesystem capability rooted there then resolves the
// mention to a real file. The `@` is preserved as the visible marker — only the
// path is expanded.

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

// expandMentions rewrites relative @-mentions in a spawn input to full paths
// under $AURORA_WORKDIR: `@file.txt` becomes `@<workdir>/file.txt`. When
// $AURORA_WORKDIR is unset the input is returned unchanged. A mention that is
// already absolute (`@/etc/hosts`) or home-anchored (`@~/x`) is left exactly as
// the user wrote it — only relative mentions are resolved against the working
// directory.
func expandMentions(input string) string {
	workdir := strings.TrimSpace(os.Getenv("AURORA_WORKDIR"))
	if workdir == "" {
		return input
	}
	if abs, err := filepath.Abs(workdir); err == nil {
		workdir = abs
	}
	return mentionPattern.ReplaceAllStringFunc(input, func(match string) string {
		// The match holds the leading boundary (start-of-string, so "", or the
		// single whitespace rune) plus the @path; keep the boundary verbatim.
		at := strings.IndexByte(match, '@')
		lead, path := match[:at], match[at+1:]
		if path == "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") {
			return match
		}
		return lead + "@" + filepath.ToSlash(filepath.Join(workdir, path))
	})
}
