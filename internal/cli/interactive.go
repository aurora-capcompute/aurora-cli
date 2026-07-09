package cli

// The interactive shell (`aurora --interactive`). Aurora's virtual filesystem is
// already a shell metaphor, so the REPL is thin: one long-lived app (cwd and
// server held in memory), a readline prompt with history and tab-completion,
// and each line dispatched through the very same command handlers as one-shot
// mode.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/chzyer/readline"
)

// replCommands is the verb set the completer offers and the shell recognizes on
// top of the dispatch verbs; exit/quit are shell-only.
var replCommands = []string{
	"pwd", "cd", "ls", "cat", "tail", "tree", "stat", "diff",
	"spawn", "mkdir", "mv", "kill", "retry", "approve", "deny", "resolve",
	"mount", "help", "exit", "quit",
}

// pathCommands take a path argument, so their later words complete against the
// children of the referenced directory.
var pathCommands = map[string]bool{
	"cd": true, "ls": true, "cat": true, "tail": true, "tree": true,
	"stat": true, "kill": true, "retry": true, "diff": true,
}

func (a *app) repl(_ context.Context) error {
	a.connect("")
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          a.prompt(),
		HistoryFile:     historyFile(),
		AutoComplete:    completer{a},
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return fmt.Errorf("interactive: %w", err)
	}
	defer rl.Close()

	a.printf("aurora interactive — commands without the `aurora` prefix; `help` for the list, Ctrl-D or `exit` to leave.")

	for {
		rl.SetPrompt(a.prompt())
		line, err := rl.Readline()
		switch {
		case errors.Is(err, readline.ErrInterrupt): // Ctrl-C clears the line
			continue
		case errors.Is(err, io.EOF): // Ctrl-D leaves
			return nil
		case err != nil:
			return err
		}
		fields, perr := tokenize(strings.TrimSpace(line))
		if perr != nil {
			a.printf("! %v", perr)
			continue
		}
		if len(fields) == 0 {
			continue
		}
		if command := fields[0]; command == "exit" || command == "quit" {
			return nil
		}
		a.runInteractive(fields[0], fields[1:])
	}
}

// runInteractive dispatches one command under a context a SIGINT cancels, so
// Ctrl-C stops a long follow (a spawn, a park wait) without killing the shell.
func (a *app) runInteractive(command string, rest []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := a.dispatch(ctx, command, rest); err != nil && !errors.Is(err, context.Canceled) {
		a.printf("! %v", err)
	}
}

func (a *app) prompt() string { return "aurora:" + a.cwd() + "> " }

// historyFile persists REPL history beside the context file (honoring the same
// $AURORA_CONFIG / $XDG_CONFIG_HOME resolution); "" disables persistence.
func historyFile() string {
	if base := os.Getenv("AURORA_CONFIG"); base != "" {
		return base + ".history"
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	dir = filepath.Join(dir, "aurora")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "history")
}

// tokenize splits a REPL line into words, honoring single and double quotes so
// an argument with spaces (e.g. spawn "do the thing") stays one token.
func tokenize(line string) ([]string, error) {
	var tokens []string
	var buf strings.Builder
	inWord := false
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				buf.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case unicode.IsSpace(r):
			if inWord {
				tokens = append(tokens, buf.String())
				buf.Reset()
				inWord = false
			}
		default:
			buf.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if inWord {
		tokens = append(tokens, buf.String())
	}
	return tokens, nil
}

// completer offers the command verbs for the first word and directory children
// for a path argument.
type completer struct{ app *app }

func (c completer) Do(line []rune, pos int) ([][]rune, int) {
	text := string(line[:pos])
	start := strings.LastIndexAny(text, " \t") + 1
	word := text[start:]
	if strings.TrimSpace(text[:start]) == "" { // completing the command verb
		return completeFrom(replCommands, word)
	}
	if pathCommands[strings.Fields(text)[0]] {
		return c.app.completePath(word)
	}
	return nil, 0
}

func completeFrom(candidates []string, prefix string) ([][]rune, int) {
	var out [][]rune
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, prefix) {
			out = append(out, []rune(candidate[len(prefix):]))
		}
	}
	return out, len(prefix)
}

// completePath completes a partial path against the children of its parent
// directory, resolved through the API (best-effort, short timeout).
func (a *app) completePath(word string) ([][]rune, int) {
	dir, prefix := path.Split(word)
	target := dir
	if target == "" {
		target = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if a.client == nil {
		a.connect("")
	}
	n, err := a.resolveNode(ctx, target)
	if err != nil {
		return nil, len(prefix)
	}
	entries, err := a.list(ctx, n)
	if err != nil {
		return nil, len(prefix)
	}
	var out [][]rune
	for _, entry := range entries {
		if strings.HasPrefix(entry.name, prefix) {
			out = append(out, []rune(entry.name[len(prefix):]))
		}
	}
	return out, len(prefix)
}
