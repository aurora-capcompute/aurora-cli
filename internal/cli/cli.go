// Package cli implements the aurora terminal: a shell over an aurora-dist. The
// distribution's state is a tree, so the commands are a shell's: pwd, cd, ls,
// cat, tail, stat and diff read it, spawn, mkdir, kill, retry, approve, deny and
// resolve act on it, and mount points it at a server. The working directory
// persists in a small config file, like a shell's.
//
// Reads come from one endpoint: GET /v1/sessions/{id} returns the whole
// session log, and every narrower view is computed here.
//
//	cli.go          Run, command dispatch, flags, the working directory
//	fs.go           the tree: paths to nodes, listing, file contents
//	navigate.go     pwd, cd, mount
//	read.go         ls, cat, less, tail, stat, diff
//	spawn.go        spawn and the live follow
//	act.go          mkdir, mv, kill, retry, approve, deny, resolve
//	render.go       one-line renderings, and escape-safe output
//	interactive.go  the REPL
package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
	"github.com/aurora-capcompute/aurora-cli/internal/cli/config"
)

const usage = `aurora — a shell over an aurora-dist

Run one command, or start an interactive session with the same commands:
  aurora --interactive         a REPL with a live prompt, history, and
                               tab-completion (Ctrl-D or "exit" to leave)

The distribution is a virtual filesystem:

  /                     the server: sessions, plus programs/
  /programs/agent       loaded program artifacts
  /alpha                a session (its name; unnamed ones show as /ses_x)
  /ses_x/proc_y         a process: status input answer error manifest,
                        journal positions 0 1 2 …, revisions/, tasks/
  /ses_x/proc_y/17      one journal entry (cat it)
  /ses_x/proc_y/revisions/2/17   the entry as revision 2 saw it
  /ses_x/proc_y/tasks/task_z     a durable task (ls -l shows -> its position)

Navigate and read (paths are absolute or relative to the saved cwd; unique
id prefixes resolve):
  pwd                          print the current path
  cd [path|-]                  change it (no arg: /)
  ls [path] [-l]               list a directory (-l: one detailed line each)
  cat <path>...                print a file: an entry, a task, history, …
  less <path>...               page a file through $PAGER (else less); set
                               $AURORA_PAGER to override — for long ones
  tail [path] [-n N]           the last N entries of a directory (default 10)
  stat <path>                  detailed JSON for any node
  diff <revA> <revB>           where a process's two revisions diverge

Act (history is append-only: there is no rm — these are the only writes):
  spawn <input> [-detach]      run a process in the current session and follow
                               it to its answer. The server's skills set what
                               it may do.
  mkdir [name] [-tag k=v ...]  create a session (a directory); prints its handle
                               — the name, or a generated id if unnamed
  mv <session> <new-name>      rename a session
  kill [process]               stop a process (default: the cwd's)
  retry [-restart] [process]   resume (default) or restart a process
  approve <task> [-reason t]   resolve a pending task as approved
  deny <task> [-reason t]      resolve a pending task as denied
  resolve <task> -decision d [-data json] [-reason t] [-token t]
  mount [url]                  print or set the aurora-dist server

The server resolves from -server, else the mounted context, else
$AURORA_DIST, else http://127.0.0.1:8080.`

// Run executes one command line; out receives human output.
func Run(ctx context.Context, args []string, out io.Writer) error {
	saved, err := config.Load()
	if err != nil {
		return fmt.Errorf("load context: %w", err)
	}
	a := &app{ctx: saved, out: out}
	if len(args) > 0 && isInteractiveFlag(args[0]) {
		return a.repl(ctx)
	}
	if len(args) == 0 {
		return a.whereami(ctx)
	}
	return a.dispatch(ctx, args[0], args[1:])
}

func isInteractiveFlag(s string) bool {
	switch s {
	case "-i", "-interactive", "--interactive", "interactive":
		return true
	}
	return false
}

// dispatch runs one command by name — the shared core of both one-shot mode and
// the interactive REPL.
func (a *app) dispatch(ctx context.Context, command string, rest []string) error {
	switch command {
	case "pwd":
		return a.pwd(ctx, rest)
	case "cd":
		return a.cd(ctx, rest)
	case "ls":
		return a.ls(ctx, rest)
	case "cat":
		return a.cat(ctx, rest)
	case "less":
		return a.less(ctx, rest)
	case "tail":
		return a.tail(ctx, rest)
	case "stat":
		return a.stat(ctx, rest)
	case "diff":
		return a.diff(ctx, rest)
	case "spawn":
		return a.spawn(ctx, rest)
	case "mkdir":
		return a.mkdir(ctx, rest)
	case "mv":
		return a.mv(ctx, rest)
	case "kill":
		return a.kill(ctx, rest)
	case "retry":
		return a.retry(ctx, rest)
	case "approve":
		return a.resolveShorthand(ctx, rest, "approve", "approved")
	case "deny":
		return a.resolveShorthand(ctx, rest, "deny", "denied")
	case "resolve":
		return a.resolve(ctx, rest)
	case "mount":
		return a.mount(ctx, rest)
	case "help", "-h", "--help":
		fmt.Fprintln(a.out, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q (try `help`)", command)
	}
}

type app struct {
	ctx    config.Context
	out    io.Writer
	client *client.Client
}

func (a *app) printf(format string, args ...any) {
	fmt.Fprintf(a.out, format+"\n", args...)
}

// flags builds a command flagset carrying the -server override; the caller
// registers command-specific flags on the returned set before parsing.
func (a *app) flags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.out)
	server := fs.String("server", "", "aurora-dist base URL (overrides the mounted server)")
	return fs, server
}

// bind parses a command's flags — interleaved with positionals, so flags may
// appear before or after the message/path (stdlib flag stops at the first
// positional; this permutes) — then resolves and attaches the API client.
func (a *app) bind(fs *flag.FlagSet, server *string, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	a.connect(*server)
	return positional, nil
}

// connect resolves the server — the flag override, else the mounted context,
// else $AURORA_DIST, else the local default — and attaches the API client.
func (a *app) connect(override string) {
	a.client = client.New(cmp.Or(override, a.ctx.Server, os.Getenv("AURORA_DIST"), "http://127.0.0.1:8080"))
}

func (a *app) cwd() string {
	if a.ctx.Path == "" {
		return "/"
	}
	return a.ctx.Path
}

// cwdSession returns the session id component of the current path.
func (a *app) cwdSession() (string, error) {
	segs := pathSegments(a.cwd())
	if len(segs) == 0 || segs[0] == "programs" {
		return "", errors.New("not inside a session: mkdir one and cd into it")
	}
	return segs[0], nil
}

// cwdProcess returns the process id component of the current path.
func (a *app) cwdProcess() (string, error) {
	segs := pathSegments(a.cwd())
	if len(segs) < 2 || segs[0] == "programs" || segs[1] == "history" {
		return "", errors.New("not inside a process: cd into one, or pass its path")
	}
	return segs[1], nil
}

func (a *app) emitJSON(value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	a.printf("%s", raw)
	return nil
}

// --- navigation ---

// --- reads ---

// --- verbs ---

// --- tasks ---

// --- rendering helpers ---
