// Package cli implements the aurora terminal: a shell over an aurora-dist.
// The distribution's event-sourced state is a tree (see fs.go), so the
// commands are the shell's own — pwd/cd/ls/cat/tail/tree/stat/diff to read
// it, spawn/mkdir/kill/retry/approve/deny/resolve to act on it, mount to point
// at a server. It keeps a working directory the way a shell does, persisted
// in a small config file so a path chosen once need not be retyped. Reads
// come off one endpoint — GET /v1/sessions/{id} returns the whole session
// log — and every narrower view is computed here in the terminal.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
	"github.com/aurora-capcompute/aurora-cli/internal/cli/config"
)

const usage = `aurora — a shell over an aurora-dist

The distribution is a virtual filesystem:

  /                     the tenant: sessions, plus programs/
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
  tail [path] [-n N]           the last N entries of a directory (default 10)
  tree [path]                  the delegation tree of processes
  stat <path>                  detailed JSON for any node
  diff <revA> <revB>           where a process's two revisions diverge

Act (history is append-only: there is no rm — these are the only writes):
  spawn <input> [-manifest f|-] [-detach]
                               run a process in the current session and follow
                               it to its answer. Manifest: -manifest, else
                               $AURORA_MANIFEST, else none — never inherited.
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
	if len(args) == 0 {
		return a.whereami(ctx)
	}

	command, rest := args[0], args[1:]
	switch command {
	case "pwd":
		return a.pwd(ctx, rest)
	case "cd":
		return a.cd(ctx, rest)
	case "ls":
		return a.ls(ctx, rest)
	case "cat":
		return a.cat(ctx, rest)
	case "tail":
		return a.tail(ctx, rest)
	case "tree":
		return a.tree(ctx, rest)
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
		return a.resolveShorthand(ctx, rest, "approved")
	case "deny":
		return a.resolveShorthand(ctx, rest, "denied")
	case "resolve":
		return a.resolve(ctx, rest)
	case "mount":
		return a.mount(ctx, rest)
	case "help", "-h", "--help":
		fmt.Fprintln(out, usage)
		return nil
	default:
		fmt.Fprintln(out, usage)
		return fmt.Errorf("unknown command %q", command)
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
	base := override
	if base == "" {
		base = a.ctx.Server
	}
	if base == "" {
		base = os.Getenv("AURORA_DIST")
	}
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	a.client = client.New(base)
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

func marshalIndent(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}

func (a *app) emitJSON(value any) error {
	raw, err := marshalIndent(value)
	if err != nil {
		return err
	}
	a.printf("%s", raw)
	return nil
}

// whereami is the bare `aurora` invocation: the mount and the cwd.
func (a *app) whereami(context.Context) error {
	a.connect("")
	a.printf("%s on / type aurora-dist", a.client.BaseURL)
	a.printf("%s", a.cwd())
	return nil
}

// --- navigation ---

func (a *app) pwd(ctx context.Context, args []string) error {
	fs, server := a.flags("pwd")
	if _, err := a.bind(fs, server, args); err != nil {
		return err
	}
	_ = ctx
	a.printf("%s", a.cwd())
	return nil
}

func (a *app) cd(ctx context.Context, args []string) error {
	fs, server := a.flags("cd")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	target := "/"
	if len(rest) > 0 {
		target = rest[0]
	}
	if target == "-" {
		target = a.ctx.PrevPath
		if target == "" {
			target = "/"
		}
	}
	n, err := a.resolveNode(ctx, target)
	if err != nil {
		return err
	}
	if !n.isDir() {
		return notDir(n.path)
	}
	a.ctx.PrevPath = a.cwd()
	a.ctx.Path = n.path
	return config.Save(a.ctx)
}

func (a *app) mount(ctx context.Context, args []string) error {
	fs, server := a.flags("mount")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	_ = ctx
	if len(rest) == 0 {
		a.printf("%s on / type aurora-dist", a.client.BaseURL)
		return nil
	}
	// A different server is a different filesystem: reset the cwd with it.
	a.ctx.Server = strings.TrimRight(rest[0], "/")
	a.ctx.Path = ""
	a.ctx.PrevPath = ""
	return config.Save(a.ctx)
}

// --- reads ---

func (a *app) ls(ctx context.Context, args []string) error {
	fs, server := a.flags("ls")
	long := fs.Bool("l", false, "one detailed line per entry")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	target := "."
	if len(rest) > 0 {
		target = rest[0]
	}
	n, err := a.resolveNode(ctx, target)
	if err != nil {
		return err
	}
	entries, err := a.list(ctx, n)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if *long {
			a.printf("%s", entry.long)
		} else {
			a.printf("%s", entry.name)
		}
	}
	return nil
}

func (a *app) cat(ctx context.Context, args []string) error {
	fs, server := a.flags("cat")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: cat <path>...")
	}
	for _, target := range rest {
		n, err := a.resolveNode(ctx, target)
		if err != nil {
			return err
		}
		lines, err := catLines(n)
		if err != nil {
			return err
		}
		for _, line := range lines {
			a.printf("%s", line)
		}
	}
	return nil
}

// tail prints the last N entries of a directory — the most recent processes
// of a session, the newest journal entries of a process, the latest sessions
// at the root — or the last N lines of a file.
func (a *app) tail(ctx context.Context, args []string) error {
	fs, server := a.flags("tail")
	count := fs.Int("n", 10, "entries to show")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	target := "."
	if len(rest) > 0 {
		target = rest[0]
	}
	n, err := a.resolveNode(ctx, target)
	if err != nil {
		return err
	}
	var lines []string
	if n.isDir() {
		entries, err := a.list(ctx, n)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			lines = append(lines, entry.long)
		}
	} else if lines, err = catLines(n); err != nil {
		return err
	}
	if *count > 0 && len(lines) > *count {
		lines = lines[len(lines)-*count:]
	}
	for _, line := range lines {
		a.printf("%s", line)
	}
	return nil
}

func (a *app) tree(ctx context.Context, args []string) error {
	fs, server := a.flags("tree")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	target := "."
	if len(rest) > 0 {
		target = rest[0]
	}
	n, err := a.resolveNode(ctx, target)
	if err != nil {
		return err
	}
	switch n.kind {
	case nodeRoot:
		summaries, err := a.client.ListSessions(ctx)
		if err != nil {
			return err
		}
		for _, summary := range summaries {
			log, err := a.client.Session(ctx, summary.ID)
			if err != nil {
				return err
			}
			a.printf("%s  %s", log.Session.ID, quoteTitle(truncate(log.Session.Title, 48)))
			a.printTree(log, rootProcesses(log), "")
		}
		return nil
	case nodeSession:
		a.printf("%s  %s", n.log.Session.ID, quoteTitle(truncate(n.log.Session.Title, 48)))
		a.printTree(n.log, rootProcesses(n.log), "")
		return nil
	case nodeProcess:
		a.printf("%s", processLine(n.process.Process))
		a.printTree(n.log, n.process.ChildProcessIDs, "")
		return nil
	default:
		return notDir(n.path)
	}
}

// rootProcesses lists a session's top-level processes — those not spawned by
// another.
func rootProcesses(log client.SessionLog) []string {
	var roots []string
	for _, proc := range log.Processes {
		if proc.ParentProcessID == "" {
			roots = append(roots, proc.ID)
		}
	}
	return roots
}

// printTree renders processes and their spawned children with tree
// connectors.
func (a *app) printTree(log client.SessionLog, ids []string, prefix string) {
	byID := make(map[string]client.ProcessLog, len(log.Processes))
	for _, proc := range log.Processes {
		byID[proc.ID] = proc
	}
	a.printSubtree(byID, ids, prefix)
}

func (a *app) printSubtree(byID map[string]client.ProcessLog, ids []string, prefix string) {
	for i, id := range ids {
		proc, ok := byID[id]
		if !ok {
			continue
		}
		connector, childPrefix := "├── ", prefix+"│   "
		if i == len(ids)-1 {
			connector, childPrefix = "└── ", prefix+"    "
		}
		a.printf("%s%s%s", prefix, connector, processLine(proc.Process))
		a.printSubtree(byID, proc.ChildProcessIDs, childPrefix)
	}
}

func (a *app) stat(ctx context.Context, args []string) error {
	fs, server := a.flags("stat")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: stat <path>")
	}
	n, err := a.resolveNode(ctx, rest[0])
	if err != nil {
		return err
	}
	switch n.kind {
	case nodeRoot:
		summaries, err := a.client.ListSessions(ctx)
		if err != nil {
			return err
		}
		return a.emitJSON(map[string]any{"server": a.client.BaseURL, "sessions": len(summaries)})
	case nodePrograms:
		programs, err := a.client.Programs(ctx)
		if err != nil {
			return err
		}
		return a.emitJSON(map[string]any{"programs": len(programs)})
	case nodeProgram:
		return a.emitJSON(n.program)
	case nodeSession:
		return a.emitJSON(n.log.Session)
	case nodeHistory:
		return a.emitJSON(map[string]any{"turns": len(n.log.History)})
	case nodeProcess:
		return a.emitJSON(struct {
			client.Process
			Parent   string   `json:"parent_process_id,omitempty"`
			Children []string `json:"child_process_ids,omitempty"`
			Entries  int      `json:"entries"`
			Tasks    int      `json:"tasks"`
		}{n.process.Process, n.process.ParentProcessID, n.process.ChildProcessIDs,
			len(effectiveEntries(n.process.Entries, n.process.Revision)), len(n.process.Tasks)})
	case nodeProcessFile:
		lines, err := catLines(n)
		if err != nil {
			return err
		}
		return a.emitJSON(map[string]any{"file": n.file, "lines": len(lines)})
	case nodeEntry:
		return a.emitJSON(n.entry)
	case nodeRevisions:
		return a.emitJSON(map[string]any{"revisions": n.process.Revision})
	case nodeRevision:
		return a.emitJSON(map[string]any{
			"revision": n.revision,
			"entries":  len(effectiveEntries(n.process.Entries, n.revision)),
		})
	case nodeTasks:
		return a.emitJSON(map[string]any{"tasks": len(n.process.Tasks)})
	case nodeTask:
		return a.emitJSON(n.task)
	}
	return noEnt(n.path)
}

// diff shows where two revisions of one process diverge: the shared prefix
// both replayed, then the rolled-back entries (-) and their re-execution (+).
func (a *app) diff(ctx context.Context, args []string) error {
	fs, server := a.flags("diff")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return errors.New("usage: diff <process|process/revisions/N> <process|process/revisions/M>")
	}
	left, err := a.revisionView(ctx, rest[0])
	if err != nil {
		return err
	}
	right, err := a.revisionView(ctx, rest[1])
	if err != nil {
		return err
	}
	if left.process.ID != right.process.ID {
		return errors.New("diff compares two revisions of one process")
	}
	before := effectiveEntries(left.process.Entries, left.revision)
	after := effectiveEntries(right.process.Entries, right.revision)
	shared := 0
	for shared < len(before) && shared < len(after) &&
		before[shared].Position == after[shared].Position &&
		before[shared].Revision == after[shared].Revision {
		shared++
	}
	if shared == len(before) && shared == len(after) {
		a.printf("identical (%d entries)", shared)
		return nil
	}
	if shared > 0 {
		a.printf("shared prefix: #%d..#%d (%d entries)", before[0].Position, before[shared-1].Position, shared)
	}
	for _, entry := range before[shared:] {
		a.printf("- %s", renderEntry(entry, 96))
	}
	for _, entry := range after[shared:] {
		a.printf("+ %s", renderEntry(entry, 96))
	}
	return nil
}

// revisionView resolves a diff argument: a revision directory, or a process
// (meaning its current revision).
func (a *app) revisionView(ctx context.Context, arg string) (node, error) {
	n, err := a.resolveNode(ctx, arg)
	if err != nil {
		return node{}, err
	}
	switch n.kind {
	case nodeRevision:
		return n, nil
	case nodeProcess:
		n.revision = n.process.Revision
		return n, nil
	}
	return node{}, fmt.Errorf("%s: not a process or revision", n.path)
}

// --- verbs ---

func (a *app) spawn(ctx context.Context, args []string) error {
	fs, server := a.flags("spawn")
	manifestPath := fs.String("manifest", "", "path to a manifest JSON file (- reads stdin); defaults to $AURORA_MANIFEST")
	detach := fs.Bool("detach", false, "print the process id and exit instead of following")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: spawn <input> [-manifest file.json|-] [-detach]")
	}
	input := strings.Join(rest, " ")

	// The manifest is an explicit input, never inherited from the session: the
	// -manifest flag, else $AURORA_MANIFEST, else none (a no-tools run).
	manifest, err := readManifest(*manifestPath)
	if err != nil {
		return err
	}
	sessionID, err := a.cwdSession()
	if err != nil {
		return err
	}
	process, err := a.client.CreateProcess(ctx, sessionID, input, manifest)
	if err != nil {
		return err
	}
	a.printf("process %s", process.ID)
	if *detach {
		return nil
	}
	return a.pollToAnswer(ctx, process.ID)
}

// readManifest loads the manifest: the -manifest argument (a file path, or - for
// stdin), else $AURORA_MANIFEST (a file path). Empty everywhere means no
// manifest — a process with no granted tools. The manifest is never inherited
// from the session; it is an explicit input the caller sets, like an env var.
func readManifest(path string) (json.RawMessage, error) {
	if path == "" {
		path = strings.TrimSpace(os.Getenv("AURORA_MANIFEST"))
	}
	switch path {
	case "":
		return nil, nil
	case "-":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read manifest from stdin: %w", err)
		}
		return raw, nil
	default:
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		return raw, nil
	}
}

// pollToAnswer polls the process's status until it reaches a terminal state,
// printing the answer (or the failure). A process that parks on a task — an
// approval or a timer — keeps being polled: a timer resolves itself and an
// approval is resolved out-of-band, so a hint is printed once and following
// continues until the process finishes.
func (a *app) pollToAnswer(ctx context.Context, processID string) error {
	hinted := false
	for {
		process, err := a.client.GetProcess(ctx, processID)
		if err != nil {
			return err
		}
		switch {
		case process.Status == "completed":
			a.printf("✔ %s", process.Answer)
			return nil
		case process.Status == "failed":
			return fmt.Errorf("process failed: %s", process.Error)
		case process.Status == "stopped":
			a.printf("■ stopped")
			return nil
		case process.Status == "compensated":
			a.printf("↩ rolled back — %s", process.Answer)
			return nil
		case process.Status == "interrupted":
			return fmt.Errorf("process interrupted: %s", process.Error)
		case process.Parked() && !hinted:
			a.printf("⏸ %s%s; still following", process.Status, a.parkHint(ctx, process))
			hinted = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// parkHint says what a parked process actually waits on: a timer (it fires
// on its own — a nap, or an abort-retry) needs nothing, anything else needs
// a human resolution.
func (a *app) parkHint(ctx context.Context, process client.Process) string {
	log, err := a.client.Session(ctx, process.SessionID)
	if err != nil {
		return ""
	}
	for _, candidate := range log.Processes {
		if candidate.ID != process.ID {
			continue
		}
		for _, task := range candidate.Tasks {
			if task.State != "pending" {
				continue
			}
			if task.Syscall.Name != "sys.timer" {
				return fmt.Sprintf(" — task %s awaits resolution (`approve %s` or `deny`)", task.ID, task.ID)
			}
		}
		return " — waiting on a timer; it fires on its own"
	}
	return ""
}

// mkdir opens a session — a directory under the tenant. An optional name is its
// handle (unique per tenant); without one the generated id is printed as the
// handle to use.
func (a *app) mkdir(ctx context.Context, args []string) error {
	fs, server := a.flags("mkdir")
	var tags tagFlags
	fs.Var(&tags, "tag", "k=v tag (repeatable)")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(strings.Join(rest, " "))
	log, err := a.client.CreateSession(ctx, name, tags.values)
	if err != nil {
		return err
	}
	a.printf("%s", sessionHandle(log.Session))
	return nil
}

// mv renames a session — the one mutable name in the tree. Its id, and every
// path built on it, is unchanged; only the handle moves.
func (a *app) mv(ctx context.Context, args []string) error {
	fs, server := a.flags("mv")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return errors.New("usage: mv <session> <new-name>")
	}
	n, err := a.resolveNode(ctx, rest[0])
	if err != nil {
		return err
	}
	if n.kind != nodeSession {
		return fmt.Errorf("mv: only a session can be renamed (%s is not a session)", n.path)
	}
	name := strings.TrimSpace(strings.Join(rest[1:], " "))
	if _, err := a.client.RenameSession(ctx, n.log.Session.ID, name); err != nil {
		return err
	}
	return nil
}

// targetProcess resolves a verb's process: an explicit path (or bare id),
// else the cwd's process.
func (a *app) targetProcess(ctx context.Context, rest []string) (string, error) {
	if len(rest) == 0 {
		return a.cwdProcess()
	}
	if n, err := a.resolveNode(ctx, rest[0]); err == nil {
		if n.kind != nodeProcess {
			return "", fmt.Errorf("%s: not a process", n.path)
		}
		return n.process.ID, nil
	}
	// A bare id from anywhere: let the API find it.
	if process, err := a.client.GetProcess(ctx, rest[0]); err == nil {
		return process.ID, nil
	}
	return "", noEnt(canonicalize(a.cwd(), rest[0]))
}

func (a *app) kill(ctx context.Context, args []string) error {
	fs, server := a.flags("kill")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	processID, err := a.targetProcess(ctx, rest)
	if err != nil {
		return err
	}
	process, err := a.client.Stop(ctx, processID)
	if err != nil {
		return err
	}
	a.printf("%s", processLine(process))
	return nil
}

func (a *app) retry(ctx context.Context, args []string) error {
	fs, server := a.flags("retry")
	restart := fs.Bool("restart", false, "restart from scratch instead of resuming")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	processID, err := a.targetProcess(ctx, rest)
	if err != nil {
		return err
	}
	mode := "resume"
	if *restart {
		mode = "restart"
	}
	process, err := a.client.Retry(ctx, processID, mode)
	if err != nil {
		return err
	}
	a.printf("%s", processLine(process))
	return nil
}

// --- tasks ---

// targetTask resolves a task argument: a path to a task node, else a bare
// task id looked up across sessions (the trusted local terminal may roam).
func (a *app) targetTask(ctx context.Context, ref string) (client.Task, error) {
	if n, err := a.resolveNode(ctx, ref); err == nil && n.kind == nodeTask {
		return n.task, nil
	}
	if sessionID, err := a.cwdSession(); err == nil {
		if task, ok := a.taskInSession(ctx, sessionID, ref); ok {
			return task, nil
		}
	}
	sessions, err := a.client.ListSessions(ctx)
	if err != nil {
		return client.Task{}, err
	}
	for _, summary := range sessions {
		if task, ok := a.taskInSession(ctx, summary.ID, ref); ok {
			return task, nil
		}
	}
	return client.Task{}, fmt.Errorf("task %s not found", ref)
}

func (a *app) taskInSession(ctx context.Context, sessionID, taskID string) (client.Task, bool) {
	log, err := a.client.Session(ctx, sessionID)
	if err != nil {
		return client.Task{}, false
	}
	for _, process := range log.Processes {
		for _, task := range process.Tasks {
			if task.ID == taskID {
				return task, true
			}
		}
	}
	return client.Task{}, false
}

func (a *app) resolveShorthand(ctx context.Context, args []string, decision string) error {
	fs, server := a.flags(decision)
	reason := fs.String("reason", "", "resolution reason")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: %s <task> [-reason text]", map[string]string{"approved": "approve", "denied": "deny"}[decision])
	}
	task, err := a.targetTask(ctx, rest[0])
	if err != nil {
		return err
	}
	resolved, err := a.client.ResolveTask(ctx, task.ID, task.ResolutionToken, client.Resolution{
		Decision: decision,
		Actor:    "aurora",
		Reason:   *reason,
	})
	if err != nil {
		return err
	}
	a.printf("%s  %s", resolved.ID, resolved.State)
	return nil
}

func (a *app) resolve(ctx context.Context, args []string) error {
	fs, server := a.flags("resolve")
	decision := fs.String("decision", "", "approved|completed|failed|denied|cancelled")
	data := fs.String("data", "", "resolution data (JSON, for completed)")
	reason := fs.String("reason", "", "resolution reason")
	token := fs.String("token", "", "resolution token (default: looked up via the API)")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: resolve <task> -decision d [-data json] [-reason text] [-token t]")
	}
	if *decision == "" {
		return errors.New("-decision is required")
	}
	taskID, resolutionToken := rest[0], *token
	if resolutionToken == "" {
		task, err := a.targetTask(ctx, rest[0])
		if err != nil {
			return err
		}
		taskID, resolutionToken = task.ID, task.ResolutionToken
	}
	resolution := client.Resolution{Decision: *decision, Actor: "aurora", Reason: *reason}
	if *data != "" {
		resolution.Data = json.RawMessage(*data)
	}
	resolved, err := a.client.ResolveTask(ctx, taskID, resolutionToken, resolution)
	if err != nil {
		return err
	}
	a.printf("%s  %s", resolved.ID, resolved.State)
	return nil
}

// --- rendering helpers ---

func processLine(process client.Process) string {
	extra := ""
	if process.Answer != "" {
		extra = "  " + truncate(process.Answer, 60)
	}
	if process.Error != "" {
		extra = "  ! " + truncate(process.Error, 60)
	}
	return fmt.Sprintf("%s  %-16s %s%s", process.ID, process.Status, truncate(process.Input, 48), extra)
}

func renderEntry(entry client.JournalEntry, limit int) string {
	line := fmt.Sprintf("#%-3d r%-2d %-14s", entry.Position, entry.Revision, entry.Syscall.Name)
	if entry.Compensates != nil {
		line += fmt.Sprintf(" compensates #%d", *entry.Compensates)
	}
	if len(entry.Syscall.Args) > 0 {
		line += " " + compact(entry.Syscall.Args, limit)
	}
	switch entry.Outcome.Status {
	case "result":
		line += " → " + compact(entry.Outcome.Result, limit)
	case "failed":
		line += fmt.Sprintf(" ✗ %s: %s", entry.Outcome.Code, entry.Outcome.Message)
	case "yield":
		line += " ⏳ " + entry.Outcome.Message
	}
	if len(entry.Outcome.Labels) > 0 {
		line += "  [" + strings.Join(entry.Outcome.Labels, " ") + "]"
	}
	return line
}

// effectiveEntries is the journal a revision replays: for each position, the
// entry with the highest revision ≤ maxRevision (copy-on-write forks leave
// earlier-revision entries in the shared prefix).
func effectiveEntries(entries []client.JournalEntry, maxRevision uint64) []client.JournalEntry {
	best := map[int]client.JournalEntry{}
	positions := make([]int, 0, len(entries))
	for _, entry := range entries {
		if entry.Revision > maxRevision {
			continue
		}
		if current, ok := best[entry.Position]; !ok || entry.Revision > current.Revision {
			if !ok {
				positions = append(positions, entry.Position)
			}
			best[entry.Position] = entry
		}
	}
	sort.Ints(positions)
	out := make([]client.JournalEntry, 0, len(positions))
	for _, position := range positions {
		out = append(out, best[position])
	}
	return out
}

// compact renders raw JSON on one line, truncated to limit runes (0 = no
// truncation).
func compact(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	return truncate(strings.ReplaceAll(buf.String(), "\n", " "), limit)
}

func truncate(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// quoteTitle quotes a title for display, keeping it on one line.
func quoteTitle(title string) string {
	if title == "" {
		return ""
	}
	return "\"" + strings.ReplaceAll(title, "\n", " ") + "\""
}

// tagFlags collects repeated -tag k=v flags.
type tagFlags struct {
	values map[string]string
}

func (t *tagFlags) String() string { return "" }

func (t *tagFlags) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("tag %q is not k=v", value)
	}
	if t.values == nil {
		t.values = map[string]string{}
	}
	t.values[key] = val
	return nil
}
