# aurora-cli

The Aurora terminal: a **shell over an
[`aurora-dist`](https://github.com/aurora-capcompute/aurora-dist)** `/v1` API —
trusted local single-principal use, no policy layer between. It exists for two
reasons: to drive an agent from a shell, and to be the **API-completeness
test** — the client defines its own wire types and consumes only the public
HTTP contract, so anything the terminal cannot do is a hole in the API, not a
missing import.

Aurora's event-sourced state is a tree, so the terminal browses it as a
virtual filesystem — `/proc` for agents:

```
/                     the tenant: sessions, plus programs/
/programs/agent       a loaded program: cat it for the interface it bundles
                      (description + input/output schemas — what to pass)
/ses_x                a session: history + its processes
/ses_x/proc_y         a process: status input answer error manifest,
                      revisions/, tasks/
/ses_x/proc_y/revisions/3      a revision: its journal positions 0 1 2 …
/ses_x/proc_y/revisions/2/17   one journal entry, as revision 2 saw it (cat it)
/ses_x/proc_y/tasks/task_z     a durable task; ls -l shows -> its position,
                               because a task IS its open intent's park
```

The commands are the shell's own. Paths are absolute or relative to a
**saved working directory** (`$AURORA_CONFIG`, else
`$XDG_CONFIG_HOME/aurora/context.json`), and unique id prefixes resolve.

```
Navigate and read:
  pwd · cd [path|-] · ls [path] [-l] · cat <path>...
  tail [path] [-n N]           the last N entries: recent processes of a
                               session, newest journal entries of a revision
  tree [path]                  the delegation tree of processes
  stat <path>                  detailed JSON for any node
  diff <revA> <revB>           where a process's two revisions diverge —
                               the shared prefix, then - rolled back / + re-run

Act (history is append-only: there is no rm — these are the only writes):
  spawn <input> [-manifest f|-] [-detach]
  mkdir [name] [-tag k=v ...]  create a session, print its handle
  mv <session> <new-name>      rename a session
  kill [process] · retry [-restart] [process]
  approve <task> [-reason] · deny <task> [-reason]
  resolve <task> -decision d [-data json] [-reason] [-token t]
  mount [url]                  print or set the aurora-dist server
```

**One read, rendered here.** Every view — `ls`, `cat`, `tree`, `tail`,
`diff`, per-revision — is a rendering of the single `GET /v1/sessions/{id}`
payload the server returns; the terminal does the grouping. Only the root and
`/programs` need their own listings. All watching is a re-run away — output
is line-oriented and pipeable, so external tools (`jq`, `tv`, `grep`) compose.

`spawn` starts a process in the current session, then polls its status to
completion and prints the final answer. The manifest — the process's syscall
grant set — comes from `-manifest` (a file, or `-` for stdin), else the
`$AURORA_MANIFEST` file, else none; it is **never inherited** from an earlier
spawn. Point `$AURORA_MANIFEST` at a file to state the grants once, like an
environment. In the spawn input, an `@file.txt` mention becomes a Markdown link
to its full path under `$AURORA_WORKDIR` — `@file.txt` becomes
`[@file.txt](/work/dir/file.txt)`, the shape most agent harnesses use for an
attached file — so an agent granted a filesystem capability rooted there can
open what you name. In the interactive shell, typing `@` and a partial name
completes against those same files (`@` then Tab searches `$AURORA_WORKDIR`),
and the link is formed when you run the line. A mention already absolute
(`@/etc/hosts`) or home-anchored (`@~/x`) is left untouched, and with
`$AURORA_WORKDIR` unset nothing is rewritten. A process
parked on a durable task keeps being polled — a timer resumes it by itself,
an approval can arrive from another terminal — with a one-line hint naming
the pending task. `kill` maps to stop: a process mid-rollback refuses it by
design (effects must settle), and the honest way out is denying its pending
inverse task.

Task resolution authenticates with the task's bearer `resolution_token`; the
CLI looks tokens up through the API (the trusted-client posture) so
`approve <task-id>` — or `approve proc_y/tasks/task_z` — is enough, while
`resolve -token …` keeps the explicit path for scripts.

## Example

```sh
aurora mount http://127.0.0.1:8080     # remembered from now on
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
aurora mkdir naptime                    # create a session, prints its handle
aurora cd naptime                       # enter it
export AURORA_MANIFEST=manifest.json    # grants for the spawns that follow
aurora spawn "take a nap, then report back"
aurora spawn "now do it again"          # same manifest via $AURORA_MANIFEST
aurora ls -l          # the session: history + its processes
aurora cd proc        # unique prefix resolves to the process
aurora ls -l          # the journal narrative, one line per syscall
aurora cat answer
aurora tail -n 5 /    # the most recent sessions
```

## Verification

```sh
go vet ./...
go test -race ./...
```

The end-to-end tests build the sibling `aurora-dist` binary and the real Rust
agent program from the sibling `aurora-brains` checkout, then drive the whole
stack through this CLI — the spawn/poll loop with a firing timer, the
filesystem walk (pwd/cd/ls/cat/tail/tree/diff over sessions, processes,
journal entries, tasks, programs), and the approve/deny cycle — skipping
when the
toolchains or checkouts are absent. The module itself is pure stdlib.
