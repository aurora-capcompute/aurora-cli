// Package cli implements the aurora-cli commands: a terminal binding directly
// to an aurora-dist /v1 API. It carries a saved working context (server,
// current session, current process) the way kubectl does, so a session or
// process chosen once need not be retyped; -server/-s/-p override it per
// command. Reads all come off one endpoint — GET /v1/sessions/{id} returns the
// whole session log — and this package renders the journal, call graph, task
// list, and per-revision views from that single payload.
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

	"github.com/aurora-capcompute/aurora-cli/internal/client"
	"github.com/aurora-capcompute/aurora-cli/internal/config"
)

const usage = `aurora-cli — terminal for an aurora-dist

Usage: aurora-cli <command> [args] [-s session] [-p process] [-server url] [-o json]

Context (kubectl-style; saved so you don't retype ids):
  context                        show the current server, session, process
  use [session] [-server url]    set the current session and/or server
  new [-tag k=v ...] [-keep]     create a session and switch to it
  sessions                       list sessions (current marked with *)

Work in the current session:
  send <message> [-manifest f] [-new] [-detach]
                                 start a process and poll it to its answer
  ps                             list the session's processes
  log [--all-revisions]          the whole session log (every process)
  graph                          the delegation call-graph tree

Work on the current process (override with -p):
  proc                           show one process's status
  journal [--all-revisions]      render one process's journal
  tasks                          list tasks (pending ones show their token)
  approve <task> [-reason text]  resolve a pending task as approved
  deny <task> [-reason text]     resolve a pending task as denied
  resolve <task> -decision d [-data json] [-reason text] [-token t]
  stop                           stop the process
  retry [-restart]               resume (default) or restart the process

The server resolves from -server, else the saved context, else $AURORA_DIST,
else http://127.0.0.1:8080. -o json prints the raw payload instead of a table.`

// Run executes one command line; out receives human output.
func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return (&app{out: out}).context(ctx, nil)
	}
	saved, err := config.Load()
	if err != nil {
		return fmt.Errorf("load context: %w", err)
	}
	a := &app{ctx: saved, out: out}

	command, rest := args[0], args[1:]
	switch command {
	case "context":
		return a.context(ctx, rest)
	case "use":
		return a.use(ctx, rest)
	case "new":
		return a.newSession(ctx, rest)
	case "sessions":
		return a.sessions(ctx, rest)
	case "send":
		return a.send(ctx, rest)
	case "ps", "processes":
		return a.ps(ctx, rest)
	case "log":
		return a.log(ctx, rest)
	case "graph":
		return a.graph(ctx, rest)
	case "proc", "status":
		return a.proc(ctx, rest)
	case "journal", "j":
		return a.journal(ctx, rest)
	case "tasks":
		return a.tasks(ctx, rest)
	case "approve":
		return a.resolveShorthand(ctx, rest, "approved")
	case "deny":
		return a.resolveShorthand(ctx, rest, "denied")
	case "resolve":
		return a.resolve(ctx, rest)
	case "stop":
		return a.stop(ctx, rest)
	case "retry":
		return a.retry(ctx, rest)
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

// shared holds the flags every command accepts.
type shared struct {
	server  *string
	session *string
	process *string
	output  *string
}

// flags builds a command flagset carrying the shared overrides; the caller
// registers command-specific flags on the returned set before parsing.
func (a *app) flags(name string) (*flag.FlagSet, *shared) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.out)
	s := &shared{
		server:  fs.String("server", "", "aurora-dist base URL (overrides the saved context)"),
		session: fs.String("s", "", "session id (overrides the current session)"),
		process: fs.String("p", "", "process id (overrides the current process)"),
		output:  fs.String("o", "", "output format: json"),
	}
	return fs, s
}

// bind parses a command's flags — interleaved with positionals, so flags may
// appear before or after the message/id (stdlib flag stops at the first
// positional; this permutes) — then resolves and attaches the API client.
func (a *app) bind(fs *flag.FlagSet, s *shared, args []string) ([]string, error) {
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
	server := *s.server
	if server == "" {
		server = a.ctx.Server
	}
	if server == "" {
		server = os.Getenv("AURORA_DIST")
	}
	if server == "" {
		server = "http://127.0.0.1:8080"
	}
	a.client = client.New(server)
	return positional, nil
}

// resolveSession returns the session to act on: the -s override, else the
// saved current session.
func (a *app) resolveSession(s *shared) (string, error) {
	if *s.session != "" {
		return *s.session, nil
	}
	if a.ctx.Session != "" {
		return a.ctx.Session, nil
	}
	return "", errors.New("no current session; run `aurora-cli use <session>` or `aurora-cli new`")
}

// resolveProcess returns the process to act on: the -p override, else the
// saved current process, else the current session's active or most-recent
// process.
func (a *app) resolveProcess(ctx context.Context, s *shared) (string, error) {
	if *s.process != "" {
		return *s.process, nil
	}
	if a.ctx.Process != "" {
		return a.ctx.Process, nil
	}
	sessionID, err := a.resolveSession(s)
	if err != nil {
		return "", errors.New("no current process; run `aurora-cli send <message>` or pass -p")
	}
	log, err := a.client.Session(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if log.Session.ActiveProcessID != "" {
		return log.Session.ActiveProcessID, nil
	}
	if n := len(log.Processes); n > 0 {
		return log.Processes[n-1].ID, nil
	}
	return "", errors.New("the current session has no processes yet")
}

func (a *app) emitJSON(value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	a.printf("%s", raw)
	return nil
}

// --- context ---

func (a *app) context(ctx context.Context, args []string) error {
	fs, s := a.flags("context")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	_ = rest
	if *s.output == "json" {
		return a.emitJSON(a.ctx)
	}
	server := a.client.BaseURL
	a.printf("server   %s", server)
	if a.ctx.Session == "" {
		a.printf("session  (none — run `aurora-cli use <session>` or `aurora-cli new`)")
		return nil
	}
	title := ""
	if log, err := a.client.Session(ctx, a.ctx.Session); err == nil {
		title = "  " + quoteTitle(log.Session.Title)
	}
	a.printf("session  %s%s", a.ctx.Session, title)
	if a.ctx.Process != "" {
		a.printf("process  %s", a.ctx.Process)
	} else {
		a.printf("process  (none — defaults to the session's active process)")
	}
	return nil
}

func (a *app) use(ctx context.Context, args []string) error {
	fs, s := a.flags("use")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	changed := false
	if *s.server != "" {
		a.ctx.Server = *s.server
		changed = true
	}
	if len(rest) > 0 {
		sessionID := rest[0]
		log, err := a.client.Session(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("session %s: %w", sessionID, err)
		}
		a.ctx.Session = sessionID
		// Point the current process at the session's active one, if any.
		a.ctx.Process = log.Session.ActiveProcessID
		changed = true
		a.printf("switched to session %s %s", sessionID, quoteTitle(log.Session.Title))
	}
	if !changed {
		return errors.New("nothing to set: pass a session id and/or -server")
	}
	if err := config.Save(a.ctx); err != nil {
		return err
	}
	if *s.server != "" && len(rest) == 0 {
		a.printf("server set to %s", a.ctx.Server)
	}
	return nil
}

func (a *app) newSession(ctx context.Context, args []string) error {
	fs, s := a.flags("new")
	var tags tagFlags
	fs.Var(&tags, "tag", "k=v tag (repeatable)")
	keep := fs.Bool("keep", false, "do not switch the current context to the new session")
	if _, err := a.bind(fs, s, args); err != nil {
		return err
	}
	log, err := a.client.CreateSession(ctx, tags.values)
	if err != nil {
		return err
	}
	if !*keep {
		a.ctx.Session = log.Session.ID
		a.ctx.Process = ""
		if err := config.Save(a.ctx); err != nil {
			return err
		}
	}
	a.printf("%s", log.Session.ID)
	return nil
}

func (a *app) sessions(ctx context.Context, args []string) error {
	fs, s := a.flags("sessions")
	if _, err := a.bind(fs, s, args); err != nil {
		return err
	}
	sessions, err := a.client.ListSessions(ctx)
	if err != nil {
		return err
	}
	if *s.output == "json" {
		return a.emitJSON(sessions)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CreatedAt.Before(sessions[j].CreatedAt) })
	if len(sessions) == 0 {
		a.printf("no sessions")
		return nil
	}
	for _, session := range sessions {
		marker := " "
		if session.ID == a.ctx.Session {
			marker = "*"
		}
		a.printf("%s %s  %-40s  %d processes", marker, session.ID, truncate(session.Title, 40), session.ProcessCount)
	}
	return nil
}

// --- processes ---

func (a *app) send(ctx context.Context, args []string) error {
	fs, s := a.flags("send")
	manifestPath := fs.String("manifest", "", "path to a manifest JSON file")
	detach := fs.Bool("detach", false, "print the process id and exit instead of following")
	newSession := fs.Bool("new", false, "create a fresh session and switch to it first")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: send <message> [-manifest file.json] [-new] [-detach]")
	}
	message := strings.Join(rest, " ")

	var manifest json.RawMessage
	if *manifestPath != "" {
		raw, err := os.ReadFile(*manifestPath)
		if err != nil {
			return fmt.Errorf("read manifest: %w", err)
		}
		manifest = raw
	}

	var sessionID string
	if *newSession {
		log, err := a.client.CreateSession(ctx, nil)
		if err != nil {
			return err
		}
		sessionID = log.Session.ID
		a.ctx.Session = sessionID
		a.printf("session %s", sessionID)
	} else {
		sessionID, err = a.resolveSession(s)
		if err != nil {
			return err
		}
	}

	process, err := a.client.CreateProcess(ctx, sessionID, message, manifest)
	if err != nil {
		return err
	}
	a.ctx.Session = sessionID
	a.ctx.Process = process.ID
	if err := config.Save(a.ctx); err != nil {
		return err
	}
	a.printf("process %s", process.ID)
	if *detach {
		return nil
	}
	return a.pollToAnswer(ctx, process.ID)
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

// parkHint says what a parked process actually waits on: a timer (it fires on
// its own — a nap, or an abort-retry) needs nothing, anything else needs a
// human resolution.
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
			if task.Syscall.Name != "timer.set" {
				return fmt.Sprintf(" — task %s awaits resolution (`approve %s` or `deny`)", task.ID, task.ID)
			}
		}
		return " — waiting on a timer; it fires on its own"
	}
	return ""
}

func (a *app) ps(ctx context.Context, args []string) error {
	fs, s := a.flags("ps")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	_ = rest
	sessionID, err := a.resolveSession(s)
	if err != nil {
		return err
	}
	log, err := a.client.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if *s.output == "json" {
		return a.emitJSON(log.Processes)
	}
	if len(log.Processes) == 0 {
		a.printf("no processes")
		return nil
	}
	for _, process := range log.Processes {
		marker := " "
		if process.ID == a.ctx.Process {
			marker = "*"
		}
		a.printf("%s %s", marker, processLine(process.Process))
	}
	return nil
}

func (a *app) proc(ctx context.Context, args []string) error {
	fs, s := a.flags("proc")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	processID, err := a.processArg(ctx, s, rest)
	if err != nil {
		return err
	}
	process, err := a.client.GetProcess(ctx, processID)
	if err != nil {
		return err
	}
	if *s.output == "json" {
		return a.emitJSON(process)
	}
	a.printf("%s", processLine(process))
	a.printf("  session   %s", process.SessionID)
	a.printf("  message   %q", process.Message)
	a.printf("  program   %s", process.ProgramDigest)
	a.printf("  journal   %d records (revision %d, attempt %d)", process.JournalLength, process.Revision, process.Attempt)
	if process.Answer != "" {
		a.printf("  answer    %s", process.Answer)
	}
	if process.Error != "" {
		a.printf("  error     %s", process.Error)
	}
	return nil
}

// --- reads (rendered from the session log) ---

func (a *app) log(ctx context.Context, args []string) error {
	fs, s := a.flags("log")
	allRevisions := fs.Bool("all-revisions", false, "show every revision, not the effective current journal")
	if _, err := a.bind(fs, s, args); err != nil {
		return err
	}
	sessionID, err := a.resolveSession(s)
	if err != nil {
		return err
	}
	log, err := a.client.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if *s.output == "json" {
		return a.emitJSON(log)
	}
	a.printf("session %s  %s", log.Session.ID, truncate(log.Session.Title, 60))
	if len(log.History) > 0 {
		a.printf("history:")
		for _, message := range log.History {
			a.printf("  %s: %s", message.Role, message.Content)
		}
	}
	for _, process := range log.Processes {
		a.printf("")
		a.printf("%s", processLine(process.Process))
		a.renderJournal(process, *allRevisions)
	}
	return nil
}

func (a *app) journal(ctx context.Context, args []string) error {
	fs, s := a.flags("journal")
	allRevisions := fs.Bool("all-revisions", false, "show every revision, not the effective current journal")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	processID, err := a.processArg(ctx, s, rest)
	if err != nil {
		return err
	}
	process, err := a.findProcessLog(ctx, s, processID)
	if err != nil {
		return err
	}
	if *s.output == "json" {
		return a.emitJSON(process)
	}
	a.renderJournal(process, *allRevisions)
	return nil
}

func (a *app) graph(ctx context.Context, args []string) error {
	fs, s := a.flags("graph")
	if _, err := a.bind(fs, s, args); err != nil {
		return err
	}
	sessionID, err := a.resolveSession(s)
	if err != nil {
		return err
	}
	log, err := a.client.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if *s.output == "json" {
		return a.emitJSON(log.Processes)
	}
	byID := make(map[string]client.ProcessLog, len(log.Processes))
	for _, process := range log.Processes {
		byID[process.ID] = process
	}
	for _, process := range log.Processes {
		if process.ParentProcessID == "" {
			a.renderGraphNode(byID, process.ID, 0)
		}
	}
	return nil
}

func (a *app) renderGraphNode(byID map[string]client.ProcessLog, id string, depth int) {
	process, ok := byID[id]
	if !ok {
		return
	}
	indent := strings.Repeat("  ", depth)
	label := process.Status
	if process.Answer != "" {
		label += " → " + truncate(process.Answer, 40)
	} else if process.Error != "" {
		label += " ! " + truncate(process.Error, 40)
	}
	a.printf("%s%s  %-16s %s", indent, id, label, truncate(process.Message, 40))
	for _, child := range process.ChildProcessIDs {
		a.renderGraphNode(byID, child, depth+1)
	}
}

func (a *app) renderJournal(process client.ProcessLog, allRevisions bool) {
	if allRevisions {
		byRevision := map[uint64][]client.JournalEntry{}
		revisions := []uint64{}
		for _, entry := range process.Entries {
			if _, ok := byRevision[entry.Revision]; !ok {
				revisions = append(revisions, entry.Revision)
			}
			byRevision[entry.Revision] = append(byRevision[entry.Revision], entry)
		}
		sort.Slice(revisions, func(i, j int) bool { return revisions[i] < revisions[j] })
		for _, revision := range revisions {
			a.printf("  revision %d:", revision)
			entries := byRevision[revision]
			sort.Slice(entries, func(i, j int) bool { return entries[i].Position < entries[j].Position })
			for _, entry := range entries {
				a.printf("    %s", renderEntry(entry, 96))
			}
		}
		return
	}
	for _, entry := range effectiveEntries(process.Entries, process.Revision) {
		a.printf("  %s", renderEntry(entry, 96))
	}
}

// effectiveEntries is the current journal a process replays: for each
// position, the entry with the highest revision ≤ the process's current
// revision (copy-on-write forks leave earlier-revision entries in the shared
// prefix). This is the grouping the server used to compute; it is derivable
// from the flat entry list the session log carries.
func effectiveEntries(entries []client.JournalEntry, maxRevision uint64) []client.JournalEntry {
	best := map[int]client.JournalEntry{}
	for _, entry := range entries {
		if entry.Revision > maxRevision {
			continue
		}
		if current, ok := best[entry.Position]; !ok || entry.Revision > current.Revision {
			best[entry.Position] = entry
		}
	}
	positions := make([]int, 0, len(best))
	for position := range best {
		positions = append(positions, position)
	}
	sort.Ints(positions)
	out := make([]client.JournalEntry, 0, len(positions))
	for _, position := range positions {
		out = append(out, best[position])
	}
	return out
}

func (a *app) stop(ctx context.Context, args []string) error {
	fs, s := a.flags("stop")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	processID, err := a.processArg(ctx, s, rest)
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
	fs, s := a.flags("retry")
	restart := fs.Bool("restart", false, "restart from scratch instead of resuming")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	processID, err := a.processArg(ctx, s, rest)
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

func (a *app) tasks(ctx context.Context, args []string) error {
	fs, s := a.flags("tasks")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	sessionID, err := a.resolveSession(s)
	if err != nil {
		return err
	}
	log, err := a.client.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	// -p (or a positional) narrows to one process; otherwise every process's
	// tasks in the session are shown, which is what a human scanning for
	// something to approve wants.
	filter := *s.process
	if filter == "" && len(rest) > 0 {
		filter = rest[0]
	}
	var tasks []client.Task
	for _, process := range log.Processes {
		if filter != "" && process.ID != filter {
			continue
		}
		tasks = append(tasks, process.Tasks...)
	}
	if *s.output == "json" {
		return a.emitJSON(tasks)
	}
	if len(tasks) == 0 {
		a.printf("no tasks")
		return nil
	}
	for _, task := range tasks {
		a.printf("%s  %-9s  %s", task.ID, task.State, task.Summary)
		a.printf("  syscall %s %s", task.Syscall.Name, compact(task.Syscall.Args, 96))
		if task.State == "pending" {
			a.printf("  token   %s", task.ResolutionToken)
		}
		if task.Resolution.Decision != "" {
			a.printf("  resolved %s by %s %s", task.Resolution.Decision, task.Resolution.Actor, task.Resolution.Reason)
		}
	}
	return nil
}

// findTask locates a task by id, preferring the current session's log and
// falling back to a tenant-wide scan (the trusted local terminal may roam).
func (a *app) findTask(ctx context.Context, s *shared, taskID string) (client.Task, error) {
	if sessionID, err := a.resolveSession(s); err == nil {
		if task, ok := a.taskInSession(ctx, sessionID, taskID); ok {
			return task, nil
		}
	}
	sessions, err := a.client.ListSessions(ctx)
	if err != nil {
		return client.Task{}, err
	}
	for _, summary := range sessions {
		if task, ok := a.taskInSession(ctx, summary.ID, taskID); ok {
			return task, nil
		}
	}
	return client.Task{}, fmt.Errorf("task %s not found", taskID)
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
	fs, s := a.flags(decision)
	reason := fs.String("reason", "", "resolution reason")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: %s <task-id> [-reason text]", map[string]string{"approved": "approve", "denied": "deny"}[decision])
	}
	task, err := a.findTask(ctx, s, rest[0])
	if err != nil {
		return err
	}
	resolved, err := a.client.ResolveTask(ctx, task.ID, task.ResolutionToken, client.Resolution{
		Decision: decision,
		Actor:    "aurora-cli",
		Reason:   *reason,
	})
	if err != nil {
		return err
	}
	a.printf("%s  %s", resolved.ID, resolved.State)
	return nil
}

func (a *app) resolve(ctx context.Context, args []string) error {
	fs, s := a.flags("resolve")
	decision := fs.String("decision", "", "approved|completed|failed|denied|cancelled")
	data := fs.String("data", "", "resolution data (JSON, for completed)")
	reason := fs.String("reason", "", "resolution reason")
	token := fs.String("token", "", "resolution token (default: looked up via the API)")
	rest, err := a.bind(fs, s, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: resolve <task-id> -decision d [-data json] [-reason text] [-token t]")
	}
	if *decision == "" {
		return errors.New("-decision is required")
	}
	resolutionToken := *token
	if resolutionToken == "" {
		task, err := a.findTask(ctx, s, rest[0])
		if err != nil {
			return err
		}
		resolutionToken = task.ResolutionToken
	}
	resolution := client.Resolution{Decision: *decision, Actor: "aurora-cli", Reason: *reason}
	if *data != "" {
		resolution.Data = json.RawMessage(*data)
	}
	resolved, err := a.client.ResolveTask(ctx, rest[0], resolutionToken, resolution)
	if err != nil {
		return err
	}
	a.printf("%s  %s", resolved.ID, resolved.State)
	return nil
}

// --- resolution + rendering helpers ---

// processArg resolves a process for a command: a positional id, else -p, else
// the saved current process, else the session's active/most-recent process.
func (a *app) processArg(ctx context.Context, s *shared, positional []string) (string, error) {
	if len(positional) > 0 {
		return positional[0], nil
	}
	return a.resolveProcess(ctx, s)
}

// findProcessLog fetches one process's ProcessLog out of its session log. It
// tries the selected/current session first, then learns the process's own
// session from a cheap snapshot — so `-p` may name a process anywhere.
func (a *app) findProcessLog(ctx context.Context, s *shared, processID string) (client.ProcessLog, error) {
	tried := map[string]bool{}
	lookup := func(sessionID string) (client.ProcessLog, bool) {
		if sessionID == "" || tried[sessionID] {
			return client.ProcessLog{}, false
		}
		tried[sessionID] = true
		log, err := a.client.Session(ctx, sessionID)
		if err != nil {
			return client.ProcessLog{}, false
		}
		for _, process := range log.Processes {
			if process.ID == processID {
				return process, true
			}
		}
		return client.ProcessLog{}, false
	}
	if process, ok := lookup(*s.session); ok {
		return process, nil
	}
	if process, ok := lookup(a.ctx.Session); ok {
		return process, nil
	}
	snapshot, err := a.client.GetProcess(ctx, processID)
	if err != nil {
		return client.ProcessLog{}, err
	}
	if process, ok := lookup(snapshot.SessionID); ok {
		return process, nil
	}
	return client.ProcessLog{}, fmt.Errorf("process %s not found", processID)
}

func processLine(process client.Process) string {
	extra := ""
	if process.Answer != "" {
		extra = "  " + truncate(process.Answer, 60)
	}
	if process.Error != "" {
		extra = "  ! " + truncate(process.Error, 60)
	}
	return fmt.Sprintf("%s  %-16s %s%s", process.ID, process.Status, truncate(process.Message, 48), extra)
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
