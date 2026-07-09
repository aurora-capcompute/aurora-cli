# aurora-cli

**Your terminal into Aurora.** `aurora` is a command‑line shell for driving an
Aurora agent server. It talks to an
[aurora-dist](https://github.com/aurora-capcompute/aurora-dist) server over its
`/v1` HTTP API and lets you start agents, watch them work, and approve or deny the
actions they request — all with familiar Unix‑style commands.

> New here? Read [What is this](#what-is-this-in-plain-words), then
> [Quick start](#quick-start-5-minutes) to run your first agent.

---

## What is this, in plain words?

Aurora keeps all of its state as a **tree of events** — sessions, the processes
(agents) inside them, each process's journal of everything it did, and any pending
approval tasks. `aurora-cli` lets you **browse that tree like a filesystem**: you
`cd` into a session, `ls` its processes, `cat` an answer, `tree` the delegation
graph. History is append‑only, so there's **no `rm`** — the only writes are creating
sessions and acting on processes and tasks.

It exists for two reasons: to **drive an agent from a shell**, and to be an
**API‑completeness test** — the client only uses the public HTTP contract, so
anything the terminal can't do is a hole in the API, not a missing feature here.

## Where this fits in the Aurora system

```
        you (a human)
              │
   aurora-cli    ◀── YOU ARE HERE
              │  HTTP /v1
         aurora-dist                         ← the server you point it at
              │  runs…
        aurora-brains                        ← the Wasm agent programs it executes
```

You need a running [aurora-dist](https://github.com/aurora-capcompute/aurora-dist)
server to talk to. Everything smart — the agent, its capabilities, its secrets —
lives on the server side; the CLI is a thin, pipeable client.

## What you can do (features)

**Navigate and read** (every view is a rendering of one `GET /v1/sessions/{id}`):

| Command | What it does |
| --- | --- |
| `pwd` · `cd [path\|-]` · `ls [path] [-l]` | move around and list the tree |
| `cat <path>...` · `less <path>...` | print / page a node (answer, manifest, journal entry, task) |
| `tail [path] [-n N]` | the last N entries — recent processes, newest journal lines |
| `tree [path]` | the delegation tree of processes |
| `stat <path>` | detailed JSON for any node |
| `diff <revA> <revB>` | where two revisions of a process diverge |

**Act** (the only writes — history is append‑only, so no `rm`):

| Command | What it does |
| --- | --- |
| `spawn <input> [-manifest f\|-] [-detach]` | run a process in the current session, poll it to its answer |
| `mkdir [name] [-tag k=v ...]` | create a session |
| `mv <session> <new-name>` | rename a session |
| `kill [process]` · `retry [-restart] [process]` | stop / resume a process |
| `approve <task> [-reason]` · `deny <task> [-reason]` | resolve a pending approval |
| `resolve <task> -decision d [-data json] [-reason] [-token t]` | general task resolution (for scripts) |
| `mount [url]` | print or set the aurora-dist server (remembered) |

Other niceties: **unique id prefixes resolve** (`cd proc` finds the one process),
paths are relative to a **saved working directory**, output is **line‑oriented and
pipeable** (`aurora ls / | jq …`), and an **interactive REPL** (`aurora -i`) gives
you a live prompt with tab‑completion for commands, paths, and `@file` mentions.

## Quick start (5 minutes)

**Prerequisites:** Go 1.26+, and a running
[aurora-dist](https://github.com/aurora-capcompute/aurora-dist) server (see its
README for a 5‑minute setup).

**Build and install:**

```sh
git clone https://github.com/aurora-capcompute/aurora-cli
cd aurora-cli
go build ./cmd/aurora      # → ./aurora
# or: go install ./cmd/aurora
```

**Run your first agent:**

```sh
aurora mount http://127.0.0.1:8080   # remembered from now on
aurora mkdir demo                    # create a session
aurora cd demo                       # enter it

export AURORA_MANIFEST=manifest.json # the agent's grant set (see aurora-dist's README)
aurora spawn "say hello"             # runs the agent, polls to its answer, prints it
```

## Example session

```sh
aurora mount http://127.0.0.1:8080
cat > manifest.json <<'EOF'
{
  "version": 4,
  "syscalls": [
    {"syscall": "sys.timer"},
    {"syscall": "core.openaiApi", "hidden": true,
     "base_url": "https://api.openai.com/v1", "api_key": "sk-…",
     "default_model": "gpt-4o-mini",
     "capabilities": [{"operation": "chat", "require_approval": false}]}
  ]
}
EOF
aurora mkdir naptime                 # create a session, prints its handle
aurora cd naptime                    # enter it
export AURORA_MANIFEST=manifest.json # grants for the spawns that follow
aurora spawn "take a nap, then report back"
aurora spawn "now do it again"       # reuses $AURORA_MANIFEST

aurora ls -l          # the session: history + its processes
aurora cd proc        # unique prefix resolves to the process
aurora ls -l          # the journal, one line per syscall
aurora cat answer     # the process's answer
aurora tail -n 5 /    # the most recent sessions
```

More real invocations:

```sh
aurora -i                                 # interactive REPL
aurora spawn "summarize @notes.txt"       # @notes.txt → $AURORA_WORKDIR/notes.txt
aurora spawn "long job" -detach           # print the process id, don't follow
aurora tree /                             # delegation tree of everything
aurora diff proc_x/revisions/1 proc_x/revisions/2
aurora approve proc_y/tasks/task_z -reason "looks good"
aurora ls / | jq .                        # output is pipeable
```

### Handy details

- **Manifests** (the syscall grant set) come from `-manifest` (a file, or `-` for
  stdin), else `$AURORA_MANIFEST`. They are **never inherited** between spawns —
  set `$AURORA_MANIFEST` once to state grants like an environment.
- **`@file` mentions** in a spawn input resolve against `$AURORA_WORKDIR`, so an
  agent granted a filesystem capability rooted there can open what you name.
- **Approvals** authenticate with the task's bearer token, which the CLI fetches
  from the API for you — so `approve <task-id>` is enough.

## Configuration

| Variable | Purpose |
| --- | --- |
| `AURORA_DIST` | Fallback server URL (default is `http://127.0.0.1:8080`) |
| `AURORA_MANIFEST` | Default manifest file for `spawn` |
| `AURORA_WORKDIR` | Base dir that `@file` mentions resolve against |
| `AURORA_CONFIG` / `XDG_CONFIG_HOME` | Where the saved context (server + cwd) is stored |
| `AURORA_PAGER` / `PAGER` | Pager for `less` |

**Server resolution order:** `-server` flag → the `mount`ed server → `$AURORA_DIST`
→ `http://127.0.0.1:8080`. There is no client auth — this is trusted, single‑user,
local use.

## Project layout

```
cmd/aurora/main.go       the binary (builds to "aurora")
internal/cli/
  cli.go                 command dispatch, handlers, the spawn/poll loop, rendering
  fs.go                  the virtual filesystem: node kinds + path resolution
  mentions.go            @-file mention resolution + completion
  interactive.go         the REPL (prompt, history, tab-completion)
  config/                persists the mounted server + working directory
  client/                the aurora-dist /v1 HTTP client (self-defined wire types)
```

## Verification

```sh
go vet ./...
go test -race ./...   # end-to-end tests build sibling aurora-dist + the Rust agent
                      # and drive the whole stack; they skip if toolchains are absent
```

The only external dependency is `github.com/chzyer/readline` (for the REPL);
everything else is pure standard library.

## Related repos

- [aurora-dist](https://github.com/aurora-capcompute/aurora-dist) — the server this CLI talks to (start here to run Aurora)
- [aurora-brains](https://github.com/aurora-capcompute/aurora-brains) — the agent programs the server runs
- [aurora-slack-connector](https://github.com/aurora-capcompute/aurora-slack-connector) — a Slack client for the same API
- [capcompute](https://github.com/aurora-capcompute/capcompute) · [aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute) — the kernel and runtime underneath
