package cli

// The distribution as a tree:
//
//	/                                the server: sessions, and programs/
//	/programs/<id>                   a loaded program
//	/<session>                       a session (its name, or its id)
//	/<session>/history               its conversation
//	/<session>/<process>             a process: status input answer error manifest,
//	                                 revisions/ and tasks/
//	/<session>/<process>/revisions/<r>/<position>   a journal entry, as revision r sees it
//	/<session>/<process>/tasks/<task>               a durable task
//
// resolveNode turns a path into a typed node, fetching what it needs: one GET
// /v1/sessions/{id} carries everything at or below a session. A segment
// resolves by exact id, else by unique prefix.

import (
	"context"
	"fmt"
	"path"
	"slices"
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

// processFiles are the leaf files every process directory carries. Journal
// entries are not here: revisions are the process's top-level object, so an
// entry is reached only through revisions/<r>/<position>.
var processFiles = []string{"status", "input", "answer", "error", "manifest"}

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
	if !strings.HasPrefix(arg, "/") {
		arg = "/" + cwd + "/" + arg
	}
	return path.Clean(arg)
}

func pathSegments(p string) []string {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// match resolves the path segment want against items by their ids: an exact
// match wins, else a unique prefix. p is the whole path, for errors.
func match[T any](items []T, id func(T) string, want, p string) (T, error) {
	var zero T
	var matches []string
	var found T
	for _, item := range items {
		if id(item) == want {
			return item, nil
		}
		if strings.HasPrefix(id(item), want) {
			matches = append(matches, id(item))
			found = item
		}
	}
	switch len(matches) {
	case 1:
		return found, nil
	case 0:
		return zero, noEnt(p)
	default:
		return zero, fmt.Errorf("%s: ambiguous — matches %s", p, strings.Join(matches, ", "))
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

	proc, err := match(log.Processes, func(proc client.ProcessLog) string { return proc.ID }, segs[1], p)
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
// file, or the revisions/ and tasks/ subtrees. Journal entries live under
// revisions/<r>/, never directly in the process directory.
func (a *app) resolveInProcess(n node, p string, segs []string) (node, error) {
	switch head := segs[0]; head {
	case "status", "input", "answer", "error", "manifest":
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
		task, err := match(n.process.Tasks, func(task client.Task) string { return task.ID }, segs[1], p)
		if err != nil {
			return node{}, err
		}
		if len(segs) > 2 {
			return node{}, notDir(p)
		}
		n.kind, n.task, n.path = nodeTask, task, n.path+"/"+task.ID
		return n, nil
	default:
		return node{}, noEnt(p)
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
	for _, entry := range revisionEntries(n.process, n.revision) {
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
	program, err := match(programs, func(program client.Program) string { return program.ID }, segs[1], p)
	if err != nil {
		return node{}, err
	}
	return node{kind: nodeProgram, path: "/programs/" + program.ID, program: program}, nil
}

// session resolves a session segment: an exact id fetches directly; else an
// exact name is the primary handle; else a unique id prefix.
func (a *app) session(ctx context.Context, seg, p string) (client.SessionLog, error) {
	if log, err := a.client.Session(ctx, seg); err == nil {
		return log, nil
	}
	summaries, err := a.client.ListSessions(ctx)
	if err != nil {
		return client.SessionLog{}, err
	}
	for _, summary := range summaries {
		if summary.Name != "" && summary.Name == seg {
			return a.client.Session(ctx, summary.ID)
		}
	}
	summary, err := match(summaries, func(summary client.SessionSummary) string { return summary.ID }, seg, p)
	if err != nil {
		return client.SessionLog{}, err
	}
	return a.client.Session(ctx, summary.ID)
}

// sessionHandle is a session's display name: its explicit name, or its id when
// unnamed.
func sessionHandle(summary client.SessionSummary) string {
	if summary.Name != "" {
		return summary.Name
	}
	return summary.ID
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
		// The server lists sessions in no order.
		slices.SortFunc(summaries, func(a, b client.SessionSummary) int { return a.CreatedAt.Compare(b.CreatedAt) })
		entries := []lsEntry{
			{name: "programs/", long: "programs/  loaded program artifacts"},
		}
		for _, summary := range summaries {
			// The session name is user-supplied and is not charset-restricted, so
			// sanitize it before it reaches the terminal (ls output and tab
			// completion both print this).
			handle := sanitizeTerminal(sessionHandle(summary))
			entries = append(entries, lsEntry{
				name: handle + "/",
				long: fmt.Sprintf("%s/  %2d processes  %s  %s",
					handle, summary.ProcessCount,
					summary.UpdatedAt.Format("2006-01-02 15:04:05"),
					quoteTitle(truncate(summary.Title, 48))),
			})
		}
		return entries, nil
	case nodePrograms:
		// The server lists programs by id.
		programs, err := a.client.Programs(ctx)
		if err != nil {
			return nil, err
		}
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
		// Only the leaf files plus revisions/ and tasks/. Journal entries are
		// not listed here — revisions are the process's top-level object, so an
		// entry appears under revisions/<r>/, never directly in the process dir.
		entries := make([]lsEntry, 0, len(processFiles)+2)
		for _, file := range processFiles {
			entries = append(entries, lsEntry{name: file, long: processFileLong(n.process, file)})
		}
		entries = append(entries,
			lsEntry{name: "revisions/", long: fmt.Sprintf("revisions/  %d", n.process.Revision)},
			lsEntry{name: "tasks/", long: fmt.Sprintf("tasks/  %d", len(n.process.Tasks))},
		)
		return entries, nil
	case nodeRevisions:
		entries := make([]lsEntry, 0, n.process.Revision)
		for revision := uint64(1); revision <= n.process.Revision; revision++ {
			count := len(revisionEntries(n.process, revision))
			entries = append(entries, lsEntry{
				name: fmt.Sprintf("%d/", revision),
				long: fmt.Sprintf("%d/  %d entries", revision, count),
			})
		}
		return entries, nil
	case nodeRevision:
		var entries []lsEntry
		for _, entry := range revisionEntries(n.process, n.revision) {
			entries = append(entries, lsEntry{name: strconv.Itoa(entry.Position), long: renderEntry(entry, 96)})
		}
		return entries, nil
	case nodeTasks:
		// The server lists a process's tasks oldest first.
		entries := make([]lsEntry, 0, len(n.process.Tasks))
		for _, task := range n.process.Tasks {
			entries = append(entries, lsEntry{name: task.ID, long: taskLine(task)})
		}
		return entries, nil
	default:
		return []lsEntry{{name: path.Base(n.path), long: fileLong(n)}}, nil
	}
}

// --- file contents ---

// catLines renders a file node's content as lines: the conversation for
// history, the bare value for a process's leaf files, pretty JSON for entries,
// tasks, and programs.
func catLines(n node) ([]string, error) {
	switch n.kind {
	case nodeHistory:
		lines := make([]string, 0, len(n.log.History))
		for _, message := range n.log.History {
			// History content is guest-authored (a process answer becomes an
			// assistant turn), so neutralize control characters before it reaches
			// the terminal — the same guard valueLines applies to a process's leaf
			// files. Without it, `cat`/`page` of a session's history is a raw
			// escape-sequence sink.
			lines = append(lines, fmt.Sprintf("%s: %s",
				sanitizeTerminal(message.Role), sanitizeTerminal(message.Content)))
		}
		return lines, nil
	case nodeProcessFile:
		switch n.file {
		case "status":
			return []string{n.process.Status}, nil
		case "input":
			return valueLines(n.process.Input), nil
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
