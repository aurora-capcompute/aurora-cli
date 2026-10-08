# aurora-cli

`aurora` is a command-line client for an
[aurora-dist](https://github.com/aurora-capcompute/aurora-dist) server. It uses
the server's `/v1` HTTP API to start agents, follow them, and approve or deny
the calls they ask for.

Aurora's state is a tree: sessions, the processes in them, each process's
journal, and its tasks. The CLI shows that tree as a filesystem. You `cd` into a
session, `ls` its processes and `cat` an answer. History is append-only, so
there is no `rm`.

The CLI uses only the public API. If the terminal cannot do something, the API
is missing it. The agent, its capabilities and its secrets all stay on the
server.

## Commands

Read (every view is a grouping of one `GET /v1/sessions/{id}`):

| Command | Effect |
| --- | --- |
| `pwd`, `cd [path\|-]`, `ls [path] [-l]` | Move around the tree and list it |
| `cat <path>...`, `less <path>...` | Print or page a node: an answer, a manifest, a journal entry, a task |
| `tail [path] [-n N]` | The last N entries |
| `stat <path>` | JSON for any node |
| `diff <revA> <revB>` | Where two revisions of a process diverge |

Act (the only writes):

| Command | Effect |
| --- | --- |
| `spawn <input> [-manifest f\|-] [-detach]` | Start a process in the current session and follow it to its answer |
| `mkdir [name] [-tag k=v ...]` | Create a session |
| `mv <session> <new-name>` | Rename a session |
| `kill [process]` | Stop a process |
| `retry [-restart] [process]` | Resume or restart a process |
| `approve <task> [-reason t]`, `deny <task> [-reason t]` | Resolve a pending approval |
| `resolve <task> -decision d [-data json] [-reason t] [-token t]` | Resolve any task (for scripts) |
| `mount [url]` | Print or set the server |

A unique id prefix resolves (`cd proc` finds the one process). Paths are
relative to a saved working directory. Output is line-oriented, so it pipes
(`aurora ls / | jq .`). `aurora -i` starts a prompt with Tab completion for
commands and paths.

The tree:

```
/                              sessions, and programs/
/programs/agent                a loaded program
/ses_x                         a session: history, processes
/ses_x/proc_y                  status input answer error manifest, revisions/, tasks/
/ses_x/proc_y/revisions/2/17   journal entry 17 as revision 2 saw it
/ses_x/proc_y/tasks/task_z     a task
```

## Quick start

Requirements: Go 1.26 or later and a running aurora-dist server (see its
README).

```sh
go build ./cmd/aurora                 # or: go install ./cmd/aurora

aurora mount http://127.0.0.1:8080    # remembered
aurora mkdir demo && aurora cd demo
export AURORA_MANIFEST=manifest.json  # the grants for each spawn
aurora spawn "take a nap, then report back"

aurora ls -l                          # the session's processes
aurora cd proc                        # a unique prefix resolves
aurora cat answer
aurora ls -l revisions/1              # the journal, one line per syscall
aurora approve tasks/task_z -reason "looks good"
```

The [aurora-dist README](https://github.com/aurora-capcompute/aurora-dist) has
a minimal manifest.

- A manifest comes from `-manifest` (a file, or `-` for stdin), else
  `$AURORA_MANIFEST`. A spawn never inherits one.
- `approve` and `deny` fetch the task's token from the API, so the task id is
  enough.

## Configuration

| Variable | Purpose |
| --- | --- |
| `AURORA_DIST` | Server URL when nothing else sets one |
| `AURORA_MANIFEST` | Default manifest for `spawn` |
| `AURORA_CONFIG`, `XDG_CONFIG_HOME` | Where the saved server and working directory live |
| `AURORA_PAGER`, `PAGER` | Pager for `less` (default `less`) |

The server comes from `-server`, else the mounted server, else `$AURORA_DIST`,
else `http://127.0.0.1:8080`. There is no client authentication: the CLI is for
trusted, local use.

## Layout

```
cmd/aurora/main.go   the binary
internal/cli/
  cli.go             commands, the spawn and follow loop, rendering
  fs.go              the tree: node kinds and path resolution
  interactive.go     the prompt, history and completion
  config/            the saved server and working directory
  client/            the /v1 client, with its own wire types
```

## Test

```sh
go vet ./...
go test -race ./...
```

The end-to-end tests build aurora-dist and the agent from sibling checkouts and
drive the whole stack. They skip when a toolchain is missing. The only
dependency outside the standard library is `github.com/chzyer/readline`.
