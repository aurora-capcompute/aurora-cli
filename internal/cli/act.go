package cli

// The commands that change state: mkdir, mv, kill, retry, approve, deny and
// resolve. History is append-only, so nothing here deletes or edits a record.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
)

// mkdir opens a session — a directory under the root. An optional name is its
// handle (unique); without one the generated id is printed as the handle to
// use.
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
	_, err = a.client.RenameSession(ctx, n.log.Session.ID, name)
	return err
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

// resolveShorthand is approve and deny: command is the verb, decision the
// resolution it sends.
func (a *app) resolveShorthand(ctx context.Context, args []string, command, decision string) error {
	fs, server := a.flags(command)
	reason := fs.String("reason", "", "resolution reason")
	rest, err := a.bind(fs, server, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: %s <task> [-reason text]", command)
	}
	task, err := a.targetTask(ctx, rest[0])
	if err != nil {
		return err
	}
	a.printf("%s  %s", decision, pendingCall(task))
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
	if len(rest) == 0 {
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
