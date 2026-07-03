// Package cli implements the aurora-cli commands: a terminal binding
// directly to the aurora-dist /v1 API — create/attach sessions, start
// processes, stream events, render journals and tasks, and resolve pending
// approvals with their resolution tokens. It is the first terminal, and by
// design the API-completeness test: everything it renders comes off the
// wire, nothing from shared server code.
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

	"github.com/aurora-capcompute/aurora-cli/internal/client"
)

const usage = `aurora-cli — terminal for an aurora-dist

Usage: aurora-cli [-server URL] <command> [args]

  sessions                       list sessions
  new [-tag k=v ...]             create a session
  send <session|new> <message> [-manifest file.json] [-detach]
                                 start a process and follow it to its answer
  watch [session]                stream a session (or the tenant firehose)
  session <session>              show a session: history and processes
  proc <process>                 show one process
  journal <process> [-revisions] [-full]
                                 render a process's journal
  tasks <process>                list a process's tasks (with tokens)
  approve <task> [-reason text]  resolve a pending task as approved
  deny <task> [-reason text]     resolve a pending task as denied
  resolve <task> -decision d [-data json] [-reason text] [-token t]
                                 resolve with an explicit decision
  stop <process>                 stop a process
  retry <process> [-restart]     resume (default) or restart a process
  programs                       list registered programs
  reload                         re-scan the programs directory
  retention                      program digests still pinned by live processes

The server URL comes from -server or AURORA_DIST (default http://127.0.0.1:8080).`

// Run executes one command line; out receives human output.
func Run(ctx context.Context, args []string, out io.Writer) error {
	global := flag.NewFlagSet("aurora-cli", flag.ContinueOnError)
	global.SetOutput(out)
	server := global.String("server", "", "aurora-dist base URL (default $AURORA_DIST or http://127.0.0.1:8080)")
	global.Usage = func() { fmt.Fprintln(out, usage) }
	if err := global.Parse(args); err != nil {
		return err
	}
	rest := global.Args()
	if len(rest) == 0 {
		fmt.Fprintln(out, usage)
		return errors.New("a command is required")
	}
	base := *server
	if base == "" {
		base = os.Getenv("AURORA_DIST")
	}
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	app := &app{client: client.New(base), out: out}

	command, rest := rest[0], rest[1:]
	switch command {
	case "sessions":
		return app.sessions(ctx)
	case "new":
		return app.newSession(ctx, rest)
	case "send":
		return app.send(ctx, rest)
	case "watch":
		return app.watch(ctx, rest)
	case "session":
		return app.session(ctx, rest)
	case "proc":
		return app.proc(ctx, rest)
	case "journal":
		return app.journal(ctx, rest)
	case "tasks":
		return app.tasks(ctx, rest)
	case "approve":
		return app.resolveShorthand(ctx, rest, "approved")
	case "deny":
		return app.resolveShorthand(ctx, rest, "denied")
	case "resolve":
		return app.resolve(ctx, rest)
	case "stop":
		return app.stop(ctx, rest)
	case "retry":
		return app.retry(ctx, rest)
	case "programs":
		return app.programs(ctx)
	case "reload":
		return app.reload(ctx)
	case "retention":
		return app.retention(ctx)
	case "help", "-h", "--help":
		fmt.Fprintln(out, usage)
		return nil
	default:
		fmt.Fprintln(out, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

type app struct {
	client *client.Client
	out    io.Writer
}

func (a *app) printf(format string, args ...any) {
	fmt.Fprintf(a.out, format+"\n", args...)
}

// --- sessions ---

func (a *app) sessions(ctx context.Context) error {
	sessions, err := a.client.ListSessions(ctx)
	if err != nil {
		return err
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CreatedAt.Before(sessions[j].CreatedAt) })
	if len(sessions) == 0 {
		a.printf("no sessions")
		return nil
	}
	for _, session := range sessions {
		active := ""
		if session.ActiveProcessID != "" {
			active = "  active " + session.ActiveProcessID
		}
		a.printf("%s  %-40q  %d processes%s", session.ID, session.Title, session.ProcessCount, active)
	}
	return nil
}

func (a *app) newSession(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	flags.SetOutput(a.out)
	var tags tagFlags
	flags.Var(&tags, "tag", "k=v tag (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	session, err := a.client.CreateSession(ctx, tags.values)
	if err != nil {
		return err
	}
	a.printf("%s", session.ID)
	return nil
}

func (a *app) session(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: session <session-id>")
	}
	session, err := a.client.GetSession(ctx, args[0])
	if err != nil {
		return err
	}
	a.printf("%s  %q", session.ID, session.Title)
	for key, value := range session.Tags {
		a.printf("  tag %s=%s", key, value)
	}
	if len(session.History) > 0 {
		a.printf("history:")
		for _, message := range session.History {
			a.printf("  %s: %s", message.Role, message.Content)
		}
	}
	if len(session.Processes) > 0 {
		a.printf("processes:")
		for _, process := range session.Processes {
			a.printf("  %s", processLine(process))
		}
	}
	return nil
}

// --- processes ---

func (a *app) send(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	flags.SetOutput(a.out)
	manifestPath := flags.String("manifest", "", "path to a manifest JSON file")
	detach := flags.Bool("detach", false, "print the process id and exit instead of following")
	// Flags may follow the positional args; parse the tail.
	if len(args) < 2 {
		return errors.New("usage: send <session-id|new> <message> [-manifest file.json] [-detach]")
	}
	sessionID, message := args[0], args[1]
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	var manifest json.RawMessage
	if *manifestPath != "" {
		raw, err := os.ReadFile(*manifestPath)
		if err != nil {
			return fmt.Errorf("read manifest: %w", err)
		}
		manifest = raw
	}
	if sessionID == "new" {
		session, err := a.client.CreateSession(ctx, nil)
		if err != nil {
			return err
		}
		sessionID = session.ID
		a.printf("session %s", sessionID)
	}

	// Subscribe before creating the process so no event is missed.
	var events <-chan client.Event
	if !*detach {
		streamCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var err error
		events, err = a.client.SessionEvents(streamCtx, sessionID)
		if err != nil {
			return err
		}
	}
	process, err := a.client.CreateProcess(ctx, sessionID, message, manifest)
	if err != nil {
		return err
	}
	a.printf("process %s", process.ID)
	if *detach {
		return nil
	}
	return a.follow(ctx, events, process.ID)
}

// follow renders a session stream until the given process reaches a terminal
// state. A parked process keeps the stream open — approvals and timers resume
// it out-of-band — but pending tasks are surfaced with resolution hints.
func (a *app) follow(ctx context.Context, events <-chan client.Event, processID string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				return errors.New("event stream closed")
			}
			done, err := a.renderFollow(event, processID)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

func (a *app) renderFollow(event client.Event, processID string) (bool, error) {
	switch event.Type {
	case "snapshot":
		return false, nil
	case "progress":
		var progress struct {
			ProcessID string `json:"process_id"`
			Message   string `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &progress); err == nil && progress.ProcessID == processID {
			a.printf("  · %s", progress.Message)
		}
	case "process.updated":
		var process client.Process
		if err := json.Unmarshal(event.Data, &process); err != nil || process.ID != processID {
			return false, nil
		}
		switch {
		case process.Status == "completed":
			a.printf("✔ %s", process.Answer)
			return true, nil
		case process.Status == "failed":
			return true, fmt.Errorf("process failed: %s", process.Error)
		case process.Status == "stopped":
			a.printf("■ stopped")
			return true, nil
		case process.Status == "interrupted":
			return true, fmt.Errorf("process interrupted: %s", process.Error)
		case process.Parked():
			a.printf("⏸ %s", process.Status)
		}
	case "task.created":
		var task client.Task
		if err := json.Unmarshal(event.Data, &task); err == nil && task.ProcessID == processID {
			a.printf("⏳ task %s: %s", task.ID, task.Summary)
			if task.Syscall.Name != "timer.set" {
				a.printf("   resolve with: aurora-cli approve %s   (or deny)", task.ID)
			}
		}
	case "task.updated":
		var task client.Task
		if err := json.Unmarshal(event.Data, &task); err == nil && task.ProcessID == processID {
			a.printf("⏳ task %s → %s", task.ID, task.State)
		}
	}
	return false, nil
}

func (a *app) watch(ctx context.Context, args []string) error {
	if len(args) >= 1 {
		events, err := a.client.SessionEvents(ctx, args[0])
		if err != nil {
			return err
		}
		for event := range events {
			a.printf("%s", renderEvent(event.Type, event.Data))
		}
		return nil
	}
	events, err := a.client.Firehose(ctx, 0)
	if err != nil {
		return err
	}
	for event := range events {
		var frame struct {
			Seq       uint64          `json:"seq"`
			SessionID string          `json:"session_id"`
			Type      string          `json:"type"`
			Data      json.RawMessage `json:"data"`
		}
		if event.Type == "snapshot" {
			a.printf("[snapshot] %s", compact(event.Data, 160))
			continue
		}
		if err := json.Unmarshal(event.Data, &frame); err != nil {
			a.printf("[%s] %s", event.Type, compact(event.Data, 160))
			continue
		}
		a.printf("#%d %s %s", frame.Seq, frame.SessionID, renderEvent(frame.Type, frame.Data))
	}
	return nil
}

func (a *app) proc(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: proc <process-id>")
	}
	process, err := a.client.GetProcess(ctx, args[0])
	if err != nil {
		return err
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

func (a *app) journal(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("journal", flag.ContinueOnError)
	flags.SetOutput(a.out)
	revisions := flags.Bool("revisions", false, "render every revision")
	full := flags.Bool("full", false, "do not truncate args/results")
	if len(args) < 1 {
		return errors.New("usage: journal <process-id> [-revisions] [-full]")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	limit := 96
	if *full {
		limit = 0
	}
	if *revisions {
		byRevision, err := a.client.JournalRevisions(ctx, args[0])
		if err != nil {
			return err
		}
		revs := make([]uint64, 0, len(byRevision))
		for rev := range byRevision {
			revs = append(revs, rev)
		}
		sort.Slice(revs, func(i, j int) bool { return revs[i] < revs[j] })
		for _, rev := range revs {
			a.printf("revision %d:", rev)
			for _, entry := range byRevision[rev] {
				a.printf("  %s", renderEntry(entry, limit))
			}
		}
		return nil
	}
	entries, err := a.client.Journal(ctx, args[0])
	if err != nil {
		return err
	}
	for _, entry := range entries {
		a.printf("%s", renderEntry(entry, limit))
	}
	return nil
}

func (a *app) stop(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: stop <process-id>")
	}
	process, err := a.client.Stop(ctx, args[0])
	if err != nil {
		return err
	}
	a.printf("%s", processLine(process))
	return nil
}

func (a *app) retry(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("retry", flag.ContinueOnError)
	flags.SetOutput(a.out)
	restart := flags.Bool("restart", false, "restart from scratch instead of resuming")
	if len(args) < 1 {
		return errors.New("usage: retry <process-id> [-restart]")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	mode := "resume"
	if *restart {
		mode = "restart"
	}
	process, err := a.client.Retry(ctx, args[0], mode)
	if err != nil {
		return err
	}
	a.printf("%s", processLine(process))
	return nil
}

// --- tasks ---

func (a *app) tasks(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: tasks <process-id>")
	}
	tasks, err := a.client.Tasks(ctx, args[0])
	if err != nil {
		return err
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

// findTask scans sessions → processes → tasks for a task id. The API is
// process-scoped by design; the trusted local terminal may roam the tenant.
func (a *app) findTask(ctx context.Context, taskID string) (client.Task, error) {
	sessions, err := a.client.ListSessions(ctx)
	if err != nil {
		return client.Task{}, err
	}
	for _, summary := range sessions {
		session, err := a.client.GetSession(ctx, summary.ID)
		if err != nil {
			continue
		}
		for _, process := range session.Processes {
			tasks, err := a.client.Tasks(ctx, process.ID)
			if err != nil {
				continue
			}
			for _, task := range tasks {
				if task.ID == taskID {
					return task, nil
				}
			}
		}
	}
	return client.Task{}, fmt.Errorf("task %s not found", taskID)
}

func (a *app) resolveShorthand(ctx context.Context, args []string, decision string) error {
	flags := flag.NewFlagSet(decision, flag.ContinueOnError)
	flags.SetOutput(a.out)
	reason := flags.String("reason", "", "resolution reason")
	if len(args) < 1 {
		return fmt.Errorf("usage: %s <task-id> [-reason text]", map[string]string{"approved": "approve", "denied": "deny"}[decision])
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	task, err := a.findTask(ctx, args[0])
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
	flags := flag.NewFlagSet("resolve", flag.ContinueOnError)
	flags.SetOutput(a.out)
	decision := flags.String("decision", "", "approved|completed|failed|denied|cancelled")
	data := flags.String("data", "", "resolution data (JSON, for completed)")
	reason := flags.String("reason", "", "resolution reason")
	token := flags.String("token", "", "resolution token (default: looked up via the API)")
	if len(args) < 1 {
		return errors.New("usage: resolve <task-id> -decision d [-data json] [-reason text] [-token t]")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *decision == "" {
		return errors.New("-decision is required")
	}
	resolutionToken := *token
	if resolutionToken == "" {
		task, err := a.findTask(ctx, args[0])
		if err != nil {
			return err
		}
		resolutionToken = task.ResolutionToken
	}
	resolution := client.Resolution{Decision: *decision, Actor: "aurora-cli", Reason: *reason}
	if *data != "" {
		resolution.Data = json.RawMessage(*data)
	}
	resolved, err := a.client.ResolveTask(ctx, args[0], resolutionToken, resolution)
	if err != nil {
		return err
	}
	a.printf("%s  %s", resolved.ID, resolved.State)
	return nil
}

// --- programs ---

func (a *app) programs(ctx context.Context) error {
	programs, err := a.client.Programs(ctx)
	if err != nil {
		return err
	}
	if len(programs) == 0 {
		a.printf("no programs registered")
		return nil
	}
	for _, program := range programs {
		a.printf("%s  %s", program.ID, program.Digest)
	}
	return nil
}

func (a *app) reload(ctx context.Context) error {
	programs, err := a.client.ReloadPrograms(ctx)
	if err != nil {
		return err
	}
	a.printf("%d programs registered", len(programs))
	for _, program := range programs {
		a.printf("%s  %s", program.ID, program.Digest)
	}
	return nil
}

func (a *app) retention(ctx context.Context) error {
	refs, err := a.client.Retention(ctx)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		state := "decommissionable"
		if !ref.Decommissionable {
			state = fmt.Sprintf("pinned by %s", strings.Join(ref.Processes, ", "))
		}
		programs := strings.Join(ref.Programs, ", ")
		if programs == "" {
			programs = "(unregistered)"
		}
		a.printf("%s  %s  %s", ref.Digest, programs, state)
	}
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

func renderEvent(eventType string, data json.RawMessage) string {
	return fmt.Sprintf("[%s] %s", eventType, compact(data, 160))
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
