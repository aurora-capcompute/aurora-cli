# aurora-cli

The first Aurora terminal: a CLI binding directly to an
[`aurora-dist`](https://github.com/aurora-capcompute/aurora-dist) `/v1` API —
trusted local single-principal use, no policy layer between. It exists for
two reasons: to drive an agent from a shell, and to be the **API-completeness
test** — the client defines its own wire types and consumes only the public
HTTP+SSE contract, so anything the terminal cannot do is a hole in the API,
not a missing import.

```
aurora-cli [-server URL] <command> [args]        # or AURORA_DIST=http://…

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
  stop <process> · retry <process> [-restart]
  programs · reload · retention
```

`send` subscribes to the session's SSE stream before starting the process,
renders progress reports, journal appends, and pending tasks as they happen
(with `approve`/`deny` hints), and prints the final answer. A process parked
on a durable task keeps the stream open — a timer resumes it by itself, an
approval can arrive from another terminal.

Task resolution authenticates with the task's bearer `resolution_token`; the
CLI looks tokens up through the API (the trusted-client posture) so
`approve <task-id>` is enough, while `resolve -token …` keeps the explicit
path for scripts.

## Example

```sh
export AURORA_DIST=http://127.0.0.1:8080
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
aurora-cli send new "take a nap, then report back" -manifest manifest.json
```

## Verification

```sh
go vet ./...
go test -race ./...
```

The end-to-end tests build the sibling `aurora-dist` binary and the real Rust
agent program from the sibling `aurora-brains` checkout, then drive the whole
stack through this CLI — the send/follow loop with a firing timer, and the
approve/deny cycle — skipping when the toolchains or checkouts are absent.
The module itself is pure stdlib.
