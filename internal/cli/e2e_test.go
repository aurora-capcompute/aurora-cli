package cli

// The terminal end-to-end: aurora-cli driving a real aurora-dist binary
// (built from the sibling checkout) that runs the real Rust agent program
// (built from the sibling aurora-brains checkout) against a scripted
// OpenAI-compatible stub. This is D2's purpose — building the terminal is
// the API-completeness test, so everything here goes through the public
// wire: no shared code with the server beyond the HTTP contract.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	stackOnce sync.Once
	stackErr  error
	distBin   string
	agentWasm string
)

// buildStack compiles the sibling aurora-dist binary and the Rust agent
// program once per test run; tests skip when the toolchains or checkouts are
// unavailable.
func buildStack(t *testing.T) (string, string) {
	t.Helper()
	stackOnce.Do(func() {
		distDir, err := filepath.Abs("../../../aurora-dist")
		if err != nil {
			stackErr = err
			return
		}
		if _, err := os.Stat(filepath.Join(distDir, "go.mod")); err != nil {
			stackErr = fmt.Errorf("sibling aurora-dist checkout unavailable: %v", err)
			return
		}
		tmp, err := os.MkdirTemp("", "aurora-cli-e2e-*")
		if err != nil {
			stackErr = err
			return
		}
		distBin = filepath.Join(tmp, "aurora-dist")
		build := exec.Command("go", "build", "-o", distBin, "./cmd/aurora-dist")
		build.Dir = distDir
		if out, err := build.CombinedOutput(); err != nil {
			stackErr = fmt.Errorf("build aurora-dist: %v\n%s", err, out)
			return
		}

		if _, err := exec.LookPath("cargo"); err != nil {
			stackErr = fmt.Errorf("cargo not found: %v", err)
			return
		}
		brains, err := filepath.Abs("../../../aurora-brains")
		if err != nil {
			stackErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cargo := exec.CommandContext(ctx, "cargo", "build", "--release", "--target", "wasm32-wasip1", "-p", "agent")
		cargo.Dir = brains
		if out, err := cargo.CombinedOutput(); err != nil {
			stackErr = fmt.Errorf("build agent program: %v\n%s", err, out)
			return
		}
		agentWasm = filepath.Join(brains, "target", "wasm32-wasip1", "release", "agent.wasm")
	})
	if stackErr != nil {
		t.Skipf("full stack unavailable: %v", stackErr)
	}
	return distBin, agentWasm
}

// scriptedLLM asks for a 1-second timer until it has seen the timer fire,
// then finishes.
func scriptedLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reply := `{"actions":[{"action":"sys.timer","content":{"duration_seconds":1,"label":"nap"}}]}`
		if bytes.Contains(body, []byte(`fired`)) {
			reply = `{"actions":[{"action":"final","content":{"answer":"woke up after the nap"}}]}`
		}
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
}

// startDist launches the distribution binary on a free port and waits for
// its health endpoint.
func startDist(t *testing.T, bin, programsDir, dataDir string) (baseURL string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	cmd := exec.Command(bin, "-addr", addr, "-programs", programsDir, "-data", dataDir)
	cmd.Env = append(os.Environ(), "AURORA_TASK_SECRET=cli-e2e-secret")
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		if t.Failed() {
			t.Logf("aurora-dist logs:\n%s", logs.String())
		}
	})

	baseURL = "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return baseURL
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("aurora-dist never became healthy:\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// aurora executes one CLI command line and returns its rendered output. The
// server and working directory come from the saved context (set up per test
// via AURORA_CONFIG), exactly as they would for a user.
func aurora(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := Run(ctx, args, &out); err != nil {
		t.Fatalf("aurora-cli %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// mountServer isolates a per-test context file and mounts the server.
func mountServer(t *testing.T, server string) {
	t.Helper()
	t.Setenv("AURORA_CONFIG", filepath.Join(t.TempDir(), "context.json"))
	aurora(t, "mount", server)
}

func TestTerminalEndToEnd(t *testing.T) {
	bin, wasm := buildStack(t)

	programsDir := t.TempDir()
	raw, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(programsDir, "agent.wasm"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(programsDir, "agent.json"), []byte(`{"description":"the agent","input":{"type":"string"},"output":{"type":"string"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	llm := scriptedLLM(t)
	defer llm.Close()
	server := startDist(t, bin, programsDir, t.TempDir())
	mountServer(t, server)

	// A manifest file, as a user would write one.
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	manifest := fmt.Sprintf(`{
	  "version": 4,
	  "syscalls": [
	    {"syscall": "sys.timer"},
	    {"syscall": "core.openaiApi", "hidden": true,
	     "settings": {"base_url": %q, "api_key": "test", "allow_insecure_http": true,
	                  "default_model": "stub", "require_approval": false}}
	  ]
	}`, llm.URL+"/v1")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	// spawn -new creates the session, cds into it, spawns the process, polls
	// it through the timer park and fire, and prints the final answer. Flags
	// after the message exercise interleaved parsing.
	sent := aurora(t, "spawn", "-new", "take a nap, then report back", "-manifest", manifestPath)
	if !strings.Contains(sent, "session ses_") || !strings.Contains(sent, "process proc_") {
		t.Fatalf("spawn output missing ids:\n%s", sent)
	}
	if !strings.Contains(sent, "✔ woke up after the nap") {
		t.Fatalf("spawn did not follow to the answer:\n%s", sent)
	}
	if !strings.Contains(sent, "timer") {
		t.Fatalf("spawn did not surface the timer park:\n%s", sent)
	}
	sessionID := extract(t, sent, "session ")
	processID := extract(t, sent, "process ")

	// -new cd'ed into the fresh session, like a shell.
	if got := aurora(t, "pwd"); !strings.Contains(got, "/"+sessionID) {
		t.Fatalf("pwd = %q, want the new session as cwd", got)
	}

	// The root lists the session and the programs directory; the loaded
	// artifact is a file under it.
	if got := aurora(t, "ls", "/"); !strings.Contains(got, sessionID+"/") || !strings.Contains(got, "programs/") {
		t.Fatalf("ls / = %q", got)
	}
	if got := aurora(t, "ls", "/programs"); !strings.Contains(got, "agent") {
		t.Fatalf("ls /programs = %q", got)
	}

	// The session directory holds the conversation and the process.
	if got := aurora(t, "ls"); !strings.Contains(got, "history") || !strings.Contains(got, processID+"/") {
		t.Fatalf("ls (session) = %q", got)
	}
	if got := aurora(t, "cat", "history"); !strings.Contains(got, "user: take a nap") || !strings.Contains(got, "assistant: woke up") {
		t.Fatalf("cat history = %q", got)
	}

	// The process directory carries the leaf files plus revisions/ and tasks/;
	// the journal itself is one object down, under revisions/<r>/. Entries are
	// not listed directly in the process directory.
	shown := aurora(t, "ls", "-l", processID)
	for _, want := range []string{"status", "input", "answer", "revisions/", "tasks/"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("ls -l process missing %s:\n%s", want, shown)
		}
	}
	if strings.Contains(shown, "sys.input") || strings.Contains(shown, "openai.chat") {
		t.Fatalf("process dir should not list journal entries:\n%s", shown)
	}
	if got := aurora(t, "cat", processID+"/answer"); !strings.Contains(got, "woke up after the nap") {
		t.Fatalf("cat answer = %q", got)
	}

	// The journal is rendered as the narrative by ls -l on a revision.
	rev := aurora(t, "ls", "-l", processID+"/revisions/1")
	for _, want := range []string{"sys.input", "openai.chat", "sys.timer", "sys.output"} {
		if !strings.Contains(rev, want) {
			t.Fatalf("ls -l revision missing %s:\n%s", want, rev)
		}
	}
	if got := aurora(t, "cat", processID+"/revisions/1/0"); !strings.Contains(got, "sys.input") {
		t.Fatalf("cat entry 0 = %q", got)
	}

	// tail shows the newest entries — the answer being recorded.
	if got := aurora(t, "tail", "-n", "2", processID+"/revisions/1"); !strings.Contains(got, "sys.output") {
		t.Fatalf("tail revision = %q", got)
	}

	// The timer task is anchored to its journal position (a symlink in
	// ls -l) and was resolved by the timer service.
	if got := aurora(t, "ls", "-l", processID+"/tasks"); !strings.Contains(got, "-> ../") || !strings.Contains(got, "sys.timer") {
		t.Fatalf("ls -l tasks = %q", got)
	}
	taskID := extract(t, aurora(t, "ls", processID+"/tasks"), "")
	if got := aurora(t, "cat", processID+"/tasks/"+taskID); !strings.Contains(got, `"actor": "timer"`) {
		t.Fatalf("cat task = %q", got)
	}

	// tree renders the delegation tree; a lone process is a single node.
	if got := aurora(t, "tree"); !strings.Contains(got, "completed") {
		t.Fatalf("tree = %q", got)
	}

	// One revision is identical to the process's current view.
	if got := aurora(t, "diff", processID+"/revisions/1", processID); !strings.Contains(got, "identical") {
		t.Fatalf("diff = %q", got)
	}

	// cd persists like a shell: unique prefixes resolve, files read relative.
	aurora(t, "cd", processID)
	if got := aurora(t, "pwd"); !strings.Contains(got, "/"+sessionID+"/"+processID) {
		t.Fatalf("pwd after cd = %q", got)
	}
	if got := aurora(t, "cat", "status"); !strings.Contains(got, "completed") {
		t.Fatalf("cat status = %q", got)
	}
	aurora(t, "cd", "-")
	if got := aurora(t, "pwd"); !strings.Contains(got, "/"+sessionID) || strings.Contains(got, processID) {
		t.Fatalf("pwd after cd - = %q", got)
	}
}

// The HITL loop over the wire: a manifest whose LLM grant requires approval
// parks the run on a durable task; approve resumes it to the answer; a
// second turn denied fails the syscall and the run reports the failure.
func TestTerminalApproveDeny(t *testing.T) {
	bin, wasm := buildStack(t)

	programsDir := t.TempDir()
	raw, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(programsDir, "agent.wasm"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(programsDir, "agent.json"), []byte(`{"description":"the agent","input":{"type":"string"},"output":{"type":"string"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `{"actions":[{"action":"final","content":{"answer":"approved answer"}}]}`}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer llm.Close()
	server := startDist(t, bin, programsDir, t.TempDir())
	mountServer(t, server)

	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	manifest := fmt.Sprintf(`{
	  "version": 4,
	  "syscalls": [
	    {"syscall": "core.openaiApi", "hidden": true,
	     "settings": {"base_url": %q, "api_key": "test", "allow_insecure_http": true,
	                  "default_model": "stub", "require_approval": true}}
	  ]
	}`, llm.URL+"/v1")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	// Start detached: the process parks awaiting approval. -new cds into the
	// fresh session, so later paths are relative.
	sent := aurora(t, "spawn", "-new", "-detach", "-manifest", manifestPath, "ask the model something")
	processID := extract(t, sent, "process ")
	waitStatus(t, processID, "waiting_for_task")

	tasks := aurora(t, "ls", "-l", processID+"/tasks")
	if !strings.Contains(tasks, "pending") || !strings.Contains(tasks, "openai.chat") {
		t.Fatalf("tasks = %q", tasks)
	}
	taskID := extract(t, aurora(t, "ls", processID+"/tasks"), "")

	// Approve — by task path this time — and the process completes.
	if got := aurora(t, "approve", processID+"/tasks/"+taskID, "-reason", "looks fine"); !strings.Contains(got, "approved") {
		t.Fatalf("approve = %q", got)
	}
	waitStatus(t, processID, "completed")
	if got := aurora(t, "cat", processID+"/answer"); !strings.Contains(got, "approved answer") {
		t.Fatalf("answer = %q", got)
	}

	// Second turn in the same session (the cwd): deny it, by bare task id.
	sent = aurora(t, "spawn", "-detach", "and again")
	processID = extract(t, sent, "process ")
	waitStatus(t, processID, "waiting_for_task")
	taskID = extract(t, aurora(t, "ls", processID+"/tasks"), "")
	if got := aurora(t, "deny", taskID, "-reason", "not today"); !strings.Contains(got, "denied") {
		t.Fatalf("deny = %q", got)
	}
	// The denial fails the cognition syscall; the guest aborts and the
	// process finishes failed. The denial (with its reason) is recorded on the
	// task; the failure reason also reaches the process error and the journal
	// (now under revisions/, not the process directory).
	waitStatus(t, processID, "failed")
	if task := aurora(t, "cat", processID+"/tasks/"+taskID); !strings.Contains(task, "denied") || !strings.Contains(task, "not today") {
		t.Fatalf("task after deny:\n%s", task)
	}
}

// waitStatus polls the process's status file until it reads the wanted status.
func waitStatus(t *testing.T, processID, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		got := aurora(t, "cat", processID+"/status")
		if strings.Contains(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process never reached %s:\n%s", want, got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// extract pulls the first whitespace-delimited token after prefix (or the
// first token of the output when prefix is empty).
func extract(t *testing.T, output, prefix string) string {
	t.Helper()
	idx := strings.Index(output, prefix)
	if idx == -1 {
		t.Fatalf("output has no %q:\n%s", prefix, output)
	}
	rest := output[idx+len(prefix):]
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		t.Fatalf("nothing after %q:\n%s", prefix, output)
	}
	return fields[0]
}
