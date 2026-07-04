# aurora-cli

The first Aurora terminal: a CLI binding directly to an
[`aurora-dist`](https://github.com/aurora-capcompute/aurora-dist) `/v1` API —
trusted local single-principal use, no policy layer between. It exists for
two reasons: to drive an agent from a shell, and to be the **API-completeness
test** — the client defines its own wire types and consumes only the public
HTTP contract, so anything the terminal cannot do is a hole in the API,
not a missing import.

It carries a **saved working context** the way kubectl does: the server, the
current session, and the current process live in a small config file
(`$AURORA_CONFIG`, else `$XDG_CONFIG_HOME/aurora/context.json`), so a session
or process chosen once need not be retyped. `-s`/`-p`/`-server` override the
context for one command; `-o json` prints the raw payload for piping to `jq`.

```
aurora-cli <command> [args] [-s session] [-p process] [-server url] [-o json]

Context (saved, so you don't retype ids):
  context                        show the current server, session, process
  use [session] [-server url]    set the current session and/or server
  new [-tag k=v ...] [-keep]     create a session and switch to it
  sessions                       list sessions (current marked with *)

In the current session:
  send <message> [-manifest f] [-new] [-detach]
                                 start a process and poll it to its answer
  ps                             list the session's processes
  log [--all-revisions]          the whole session log (every process)
  graph                          the delegation call-graph tree

On the current process (override with -p):
  proc · journal [--all-revisions] · tasks
  approve <task> [-reason] · deny <task> [-reason]
  resolve <task> -decision d [-data json] [-reason] [-token t]
  stop · retry [-restart]
```

**One read, rendered here.** Every view — `log`, `journal`, `graph`, `tasks`,
per-revision — is a rendering of the single `GET /v1/sessions/{id}` payload
the server returns; the terminal does the grouping. There is no separate
graph/journal/tasks endpoint on the API.

`send` starts the process, then polls its status to completion, prints the
final answer, and remembers the process as current. A process parked on a
durable task keeps being polled — a timer resumes it by itself, an approval
can arrive from another terminal — with a hint to resolve pending tasks via
`tasks`/`approve`.

Task resolution authenticates with the task's bearer `resolution_token`; the
CLI looks tokens up through the API (the trusted-client posture) so
`approve <task-id>` is enough, while `resolve -token …` keeps the explicit
path for scripts.

## Example

```sh
aurora-cli use -server http://127.0.0.1:8080   # remembered from now on
cat > manifest.json <<'EOF'
{
  "version": 2,
  "tools": [
    {"name": "timer.set", "type": "core.timer"},
    {"name": "llm", "type": "core.openaiApi", "hidden": true,
     "settings": {"api_key": "sk-…", "default_model": "gpt-4o-mini",
                  "require_approval": false}}
  ]
}
EOF
aurora-cli send -new -manifest manifest.json "take a nap, then report back"
aurora-cli log        # the current session; no id retyped
aurora-cli journal    # the current process
```

## Verification

```sh
go vet ./...
go test -race ./...
```

The end-to-end tests build the sibling `aurora-dist` binary and the real Rust
agent program from the sibling `aurora-brains` checkout, then drive the whole
stack through this CLI — the send/poll loop with a firing timer, and the
approve/deny cycle — skipping when the toolchains or checkouts are absent.
The module itself is pure stdlib.
