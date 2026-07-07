package cli

// The distribution's virtual filesystem. Aurora's event-sourced state is a
// tree, so the terminal browses it as one:
//
//	/                    the tenant: sessions, plus programs/
//	/programs/agent      loaded program artifacts
//	/ses_x               a session: history + its processes
//	/ses_x/proc_y        a process: status/message/answer/error/manifest files,
//	                     journal positions 0 1 2 …, revisions/, tasks/
//	/ses_x/proc_y/17     one journal entry (the current revision's view)
//	/ses_x/proc_y/revisions/2/17   the entry as revision 2 saw it
//	/ses_x/proc_y/tasks/task_z     a durable task, anchored -> its position
//
// resolveNode turns a path into a typed node, fetching what it needs from
// the API — one GET /v1/sessions/{id} carries everything at or below a
// session. Segments resolve by exact id, else by unique prefix. The tree is
// read-only by design: history is append-only, so there is no rm, mv, or
// touch — the only writes are the verbs (spawn, kill, retry, approve, deny).

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/aurora-capcompute/aurora-cli/internal/cli/client"
)

type nodeKind int

const (
	nodeRoot nodeKind = iota
	nodePrograms
	nodeProgram
	nodeSession
	nodeHistory
	nodeProcess
	nodeProcessFile
	nodeEntry
	nodeRevisions
	nodeRevision
	nodeTasks
	nodeTask
)

// processFiles are the leaf files every process directory carries.
var processFiles = []string{"status", "message", "answer", "error", "manifest"}

// node is one resolved path: its kind plus the payloads fetched on the way
// down (the session log for anything at or below a session, the process for
// anything at or below a process).
type node struct {
	kind nodeKind
	path string // canonical absolute path, ids expanded to full

	program  client.Program
	log      client.SessionLog
	process  client.ProcessLog
	file     string // nodeProcessFile: which leaf
	revision uint64 // nodeRevision, and nodeEntry's view
	entry    client.JournalEntry
	task     client.Task
}

func (n node) isDir() bool {
	switch n.kind {
	case nodeRoot, nodePrograms, nodeSession, nodeProcess, nodeRevisions, nodeRevision, nodeTasks:
		return true
	}
	return false
}

func noEnt(p string) error  { return fmt.Errorf("%s: no such file or directory", p) }
func notDir(p string) error { return fmt.Errorf("%s: not a directory", p) }

// canonicalize joins a path argument onto the current directory and cleans
// it: absolute paths stand alone, relative ones resolve against cwd, "." and
// ".." collapse.
func canonicalize(cwd, arg string) string {
	if arg == "" {
		arg = "."
	}
	if !strings.HasPrefix(arg, "/") {
		if cwd == "" {
			cwd = "/"
		}
		arg = cwd + "/" + arg
	}
	cleaned := path.Clean(arg)
	if cleaned == "" || cleaned == "." {
		return "/"
	}
	return cleaned
}

func pathSegments(p string) []string {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// matchID resolves a segment against a set of ids: an exact match wins, else
// a unique prefix.
func matchID(ids []string, want, p string) (string, error) {
	var matches []string
	for _, id := range ids {
		if id == want {
			return id, nil
		}
		if strings.HasPrefix(id, want) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", noEnt(p)
	default:
		return "", fmt.Errorf("%s: ambiguous — matches %s", p, strings.Join(matches, ", "))
	}
}

func (a *app) resolveNode(ctx context.Context, arg string) (node, error) {
	p := canonicalize(a.ctx.Path, arg)
	segs := pathSegments(p)
	if len(segs) == 0 {
		return node{kind: nodeRoot, path: "/"}, nil
	}
	if segs[0] == "programs" {
		return a.resolveProgram(ctx, p, segs)
	}

	log, err := a.session(ctx, segs[0], p)
	if err != nil {
		return node{}, err
	}
	n := node{kind: nodeSession, path: "/" + log.Session.ID, log: log}
	if len(segs) == 1 {
		return n, nil
	}
	if segs[1] == "history" {
		if len(segs) > 2 {
			return node{}, notDir(p)
		}
		n.kind, n.path = nodeHistory, n.path+"/history"
		return n, nil
	}

	proc, err := matchProcess(log, segs[1], p)
	if err != nil {
		return node{}, err
	}
	n.kind, n.process, n.path = nodeProcess, proc, n.path+"/"+proc.ID
	if len(segs) == 2 {
		return n, nil
	}
	return a.resolveInProcess(n, p, segs[2:])
}

// resolveInProcess resolves the segments beneath a process directory: a leaf
// file, a journal position, or the revisions/ and tasks/ subtrees.
func (a *app) resolveInProcess(n node, p string, segs []string) (node, error) {
	switch head := segs[0]; head {
	case "status", "message", "answer", "error", "manifest":
		if len(segs) > 1 {
			return node{}, notDir(p)
		}
		n.kind, n.file, n.path = nodeProcessFile, head, n.path+"/"+head
		return n, nil
	case "revisions":
		n.kind, n.path = nodeRevisions, n.path+"/revisions"
		if len(segs) == 1 {
			return n, nil
		}
		revision, err := strconv.ParseUint(segs[1], 10, 64)
		if err != nil || revision < 1 || revision > n.process.Revision {
			return node{}, noEnt(p)
		}
		n.kind, n.revision, n.path = nodeRevision, revision, n.path+"/"+segs[1]
		if len(segs) == 2 {
			return n, nil
		}
		return entryNode(n, p, segs[2], segs[3:])
	case "tasks":
		n.kind, n.path = nodeTasks, n.path+"/tasks"
		if len(segs) == 1 {
			return n, nil
		}
		task, err := matchTask(n.process, segs[1], p)
		if err != nil {
			return node{}, err
		}
		if len(segs) > 2 {
			return node{}, notDir(p)
		}
		n.kind, n.task, n.path = nodeTask, task, n.path+"/"+task.ID
		return n, nil
	default:
		n.revision = n.process.Revision
		return entryNode(n, p, head, segs[1:])
	}
}

// entryNode resolves a numeric journal position within the view of
// n.revision.
func entryNode(n node, p, seg string, rest []string) (node, error) {
	if len(rest) > 0 {
		return node{}, notDir(p)
	}
	position, err := strconv.Atoi(seg)
	if err != nil {
		return node{}, noEnt(p)
	}
	for _, entry := range effectiveEntries(n.process.Entries, n.revision) {
		if entry.Position == position {
			n.kind, n.entry, n.path = nodeEntry, entry, n.path+"/"+seg
			return n, nil
		}
	}
	return node{}, noEnt(p)
}

func (a *app) resolveProgram(ctx context.Context, p string, segs []string) (node, error) {
	if len(segs) == 1 {
		return node{kind: nodePrograms, path: "/programs"}, nil
	}
	if len(segs) > 2 {
		return node{}, notDir(p)
	}
	programs, err := a.client.Programs(ctx)
	if err != nil {
		return node{}, err
	}
	ids := make([]string, 0, len(programs))
	for _, program := range programs {
		ids = append(ids, program.ID)
	}
	id, err := matchID(ids, segs[1], p)
	if err != nil {
		return node{}, err
	}
	for _, program := range programs {
		if program.ID == id {
			return node{kind: nodeProgram, path: "/programs/" + id, program: program}, nil
		}
	}
	return node{}, noEnt(p)
}

// session resolves a session segment: an exact id fetches directly, anything
// shorter matches a unique prefix of the listed ids.
func (a *app) session(ctx context.Context, seg, p string) (client.SessionLog, error) {
	if log, err := a.client.Session(ctx, seg); err == nil {
		return log, nil
	}
	summaries, err := a.client.ListSessions(ctx)
	if err != nil {
		return client.SessionLog{}, err
	}
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ID)
	}
	id, err := matchID(ids, seg, p)
	if err != nil {
		return client.SessionLog{}, err
	}
	return a.client.Session(ctx, id)
}

func matchProcess(log client.SessionLog, seg, p string) (client.ProcessLog, error) {
	ids := make([]string, 0, len(log.Processes))
	for _, proc := range log.Processes {
		ids = append(ids, proc.ID)
	}
	id, err := matchID(ids, seg, p)
	if err != nil {
		return client.ProcessLog{}, err
	}
	for _, proc := range log.Processes {
		if proc.ID == id {
			return proc, nil
		}
	}
	return client.ProcessLog{}, noEnt(p)
}

func matchTask(proc client.ProcessLog, seg, p string) (client.Task, error) {
	ids := make([]string, 0, len(proc.Tasks))
	for _, task := range proc.Tasks {
		ids = append(ids, task.ID)
	}
	id, err := matchID(ids, seg, p)
	if err != nil {
		return client.Task{}, err
	}
	for _, task := range proc.Tasks {
		if task.ID == id {
			return task, nil
		}
	}
	return client.Task{}, noEnt(p)
}

// --- listing ---

// lsEntry is one directory child: its short name (directories carry a
// trailing slash) and its detailed -l line.
type lsEntry struct {
	name string
	long string
}

// list returns a directory's children in order — or, for a file, the file
// itself, the way ls treats file arguments.
func (a *app) list(ctx context.Context, n node) ([]lsEntry, error) {
	switch n.kind {
	case nodeRoot:
		summaries, err := a.client.ListSessions(ctx)
		if err != nil {
			return nil, err
		}
		sort.Slice(summaries, func(i, j int) bool { return summaries[i].CreatedAt.Before(summaries[j].CreatedAt) })
		entries := []lsEntry{{name: "programs/", long: "programs/  loaded program artifacts"}}
		for _, summary := range summaries {
			entries = append(entries, lsEntry{
				name: summary.ID + "/",
				long: fmt.Sprintf("%s/  %2d processes  %s  %s",
					summary.ID, summary.ProcessCount,
					summary.UpdatedAt.Format("2006-01-02 15:04:05"),
					quoteTitle(truncate(summary.Title, 48))),
			})
		}
		return entries, nil
	case nodePrograms:
		programs, err := a.client.Programs(ctx)
		if err != nil {
			return nil, err
		}
		sort.Slice(programs, func(i, j int) bool { return programs[i].ID < programs[j].ID })
		entries := make([]lsEntry, 0, len(programs))
		for _, program := range programs {
			entries = append(entries, lsEntry{
				name: program.ID,
				long: fmt.Sprintf("%s  %s", program.ID, truncate(program.Digest, 16)),
			})
		}
		return entries, nil
	case nodeSession:
		entries := []lsEntry{{name: "history", long: fmt.Sprintf("history  %d turns", len(n.log.History))}}
		for _, proc := range n.log.Processes {
			long := proc.ID + "/" + strings.TrimPrefix(processLine(proc.Process), proc.ID)
			entries = append(entries, lsEntry{name: proc.ID + "/", long: long})
		}
		return entries, nil
	case nodeProcess:
		entries := make([]lsEntry, 0, len(processFiles)+2)
		for _, file := range processFiles {
			entries = append(entries, lsEntry{name: file, long: processFileLong(n.process, file)})
		}
		entries = append(entries,
			lsEntry{name: "revisions/", long: fmt.Sprintf("revisions/  %d", n.process.Revision)},
			lsEntry{name: "tasks/", long: fmt.Sprintf("tasks/  %d", len(n.process.Tasks))},
		)
		for _, entry := range effectiveEntries(n.process.Entries, n.process.Revision) {
			entries = append(entries, lsEntry{name: strconv.Itoa(entry.Position), long: renderEntry(entry, 96)})
		}
		return entries, nil
	case nodeRevisions:
		entries := make([]lsEntry, 0, n.process.Revision)
		for revision := uint64(1); revision <= n.process.Revision; revision++ {
			count := len(effectiveEntries(n.process.Entries, revision))
			entries = append(entries, lsEntry{
				name: fmt.Sprintf("%d/", revision),
				long: fmt.Sprintf("%d/  %d entries", revision, count),
			})
		}
		return entries, nil
	case nodeRevision:
		var entries []lsEntry
		for _, entry := range effectiveEntries(n.process.Entries, n.revision) {
			entries = append(entries, lsEntry{name: strconv.Itoa(entry.Position), long: renderEntry(entry, 96)})
		}
		return entries, nil
	case nodeTasks:
		tasks := append([]client.Task(nil), n.process.Tasks...)
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.Before(tasks[j].CreatedAt) })
		entries := make([]lsEntry, 0, len(tasks))
		for _, task := range tasks {
			entries = append(entries, lsEntry{name: task.ID, long: taskLine(task)})
		}
		return entries, nil
	default:
		return []lsEntry{{name: path.Base(n.path), long: fileLong(n)}}, nil
	}
}

// fileLong is a file node's -l line.
func fileLong(n node) string {
	switch n.kind {
	case nodeHistory:
		return fmt.Sprintf("history  %d turns", len(n.log.History))
	case nodeProcessFile:
		return processFileLong(n.process, n.file)
	case nodeEntry:
		return renderEntry(n.entry, 96)
	case nodeTask:
		return taskLine(n.task)
	case nodeProgram:
		return fmt.Sprintf("%-20s %s  %s", n.program.ID, truncate(n.program.Digest, 16), truncate(n.program.Description, 56))
	}
	return path.Base(n.path)
}

func processFileLong(proc client.ProcessLog, file string) string {
	switch file {
	case "status":
		return fmt.Sprintf("%-9s %s (attempt %d, revision %d)", file, proc.Status, proc.Attempt, proc.Revision)
	case "message":
		return fmt.Sprintf("%-9s %s", file, truncate(proc.Message, 72))
	case "answer":
		return fmt.Sprintf("%-9s %s", file, truncate(proc.Answer, 72))
	case "error":
		return fmt.Sprintf("%-9s %s", file, truncate(proc.Error, 72))
	case "manifest":
		return fmt.Sprintf("%-9s %s", file, compact(proc.Manifest, 72))
	}
	return file
}

// taskLine renders a task the way ls -l renders a symlink: a durable task's
// identity is the open intent at its journal position, so show the link.
func taskLine(task client.Task) string {
	line := fmt.Sprintf("%s -> ../%d  %-9s %-14s %s",
		task.ID, task.JournalPosition, task.State, task.Syscall.Name, truncate(task.Summary, 48))
	if task.Resolution.Decision != "" {
		line += fmt.Sprintf("  (resolved %s by %s)", task.Resolution.Decision, task.Resolution.Actor)
	}
	return line
}

// --- file contents ---

// catLines renders a file node's content as lines: the conversation for
// history, the bare value for a process's leaf files, pretty JSON for
// entries,
// tasks, and programs.
func catLines(n node) ([]string, error) {
	switch n.kind {
	case nodeHistory:
		lines := make([]string, 0, len(n.log.History))
		for _, message := range n.log.History {
			lines = append(lines, fmt.Sprintf("%s: %s", message.Role, message.Content))
		}
		return lines, nil
	case nodeProcessFile:
		switch n.file {
		case "status":
			return []string{n.process.Status}, nil
		case "message":
			return valueLines(n.process.Message), nil
		case "answer":
			return valueLines(n.process.Answer), nil
		case "error":
			return valueLines(n.process.Error), nil
		case "manifest":
			return jsonLines(n.process.Manifest)
		}
	case nodeEntry:
		return jsonLines(n.entry)
	case nodeTask:
		return jsonLines(n.task)
	case nodeProgram:
		return jsonLines(n.program)
	}
	if n.isDir() {
		return nil, fmt.Errorf("%s: is a directory", n.path)
	}
	return nil, noEnt(n.path)
}

func valueLines(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, "\n")
}

func jsonLines(value any) ([]string, error) {
	raw, err := marshalIndent(value)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(raw), "\n"), nil
}
