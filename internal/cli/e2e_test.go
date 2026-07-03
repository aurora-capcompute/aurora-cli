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
		cargo := exec.CommandContext(ctx, "cargo", "build", "--release", "--target", "wasm32-wasip1", "-p", "agent-brain")
		cargo.Dir = brains
		if out, err := cargo.CombinedOutput(); err != nil {
			stackErr = fmt.Errorf("build agent program: %v\n%s", err, out)
			return
		}
		agentWasm = filepath.Join(brains, "target", "wasm32-wasip1", "release", "agent_brain.wasm")
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
		reply := `{"actions":[{"action":"timer.set","content":{"duration_seconds":1,"label":"nap"}}]}`
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

// run executes one CLI command line and returns its rendered output.
func run(t *testing.T, server string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := Run(ctx, append([]string{"-server", server}, args...), &out); err != nil {
		t.Fatalf("aurora-cli %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
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
	llm := scriptedLLM(t)
	defer llm.Close()
	server := startDist(t, bin, programsDir, t.TempDir())

	// Programs arrived from the registry.
	if got := run(t, server, "programs"); !strings.Contains(got, "agent  ") {
		t.Fatalf("programs = %q", got)
	}

	// A manifest file, as a user would write one.
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	manifest := fmt.Sprintf(`{
	  "version": 2,
	  "tools": [
	    {"name": "timer.set", "type": "core.timer"},
	    {"name": "llm", "type": "core.openaiApi", "hidden": true,
	     "settings": {"base_url": %q, "api_key": "test", "allow_insecure_http": true,
	                  "default_model": "stub", "require_approval": false}}
	  ]
	}`, llm.URL+"/v1")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	// send new: creates the session, starts the process, follows the stream
	// through the timer park and the fire, and prints the final answer.
	sent := run(t, server, "send", "new", "take a nap, then report back", "-manifest", manifestPath)
	if !strings.Contains(sent, "session ses_") || !strings.Contains(sent, "process proc_") {
		t.Fatalf("send output missing ids:\n%s", sent)
	}
	if !strings.Contains(sent, "✔ woke up after the nap") {
		t.Fatalf("send did not follow to the answer:\n%s", sent)
	}
	if !strings.Contains(sent, "task") {
		t.Fatalf("send did not surface the timer task:\n%s", sent)
	}

	sessionID := extract(t, sent, "session ")
	processID := extract(t, sent, "process ")

	// The session folded the turn into history.
	shown := run(t, server, "session", sessionID)
	if !strings.Contains(shown, "user: take a nap") || !strings.Contains(shown, "assistant: woke up") {
		t.Fatalf("session missing history:\n%s", shown)
	}

	// The journal renders the full narrative.
	journal := run(t, server, "journal", processID)
	for _, want := range []string{"agent.input", "openai.chat", "timer.set", "agent.finish"} {
		if !strings.Contains(journal, want) {
			t.Fatalf("journal missing %s:\n%s", want, journal)
		}
	}

	// Tasks render with their resolution state.
	tasks := run(t, server, "tasks", processID)
	if !strings.Contains(tasks, "timer.set") || !strings.Contains(tasks, "resolved completed by timer") {
		t.Fatalf("tasks = %q", tasks)
	}

	// Retention: nothing non-terminal pins the digest anymore.
	if got := run(t, server, "retention"); !strings.Contains(got, "decommissionable") {
		t.Fatalf("retention = %q", got)
	}

	// sessions lists the session with its title.
	if got := run(t, server, "sessions"); !strings.Contains(got, sessionID) {
		t.Fatalf("sessions = %q", got)
	}
}

// The HITL loop over the wire: a manifest whose LLM grant requires approval
// parks the process on a durable task; approve resumes it to the answer; a
// second turn denied fails the syscall and the process reports the failure.
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
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `{"actions":[{"action":"final","content":{"answer":"approved answer"}}]}`}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer llm.Close()
	server := startDist(t, bin, programsDir, t.TempDir())

	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	manifest := fmt.Sprintf(`{
	  "version": 2,
	  "tools": [
	    {"name": "llm", "type": "core.openaiApi", "hidden": true,
	     "settings": {"base_url": %q, "api_key": "test", "allow_insecure_http": true,
	                  "default_model": "stub", "require_approval": true}}
	  ]
	}`, llm.URL+"/v1")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	// Start detached: the process parks awaiting approval.
	sent := run(t, server, "send", "new", "ask the model something", "-manifest", manifestPath, "-detach")
	sessionID, processID := extract(t, sent, "session "), extract(t, sent, "process ")
	waitStatus(t, server, processID, "waiting_for_task")

	tasks := run(t, server, "tasks", processID)
	if !strings.Contains(tasks, "pending") || !strings.Contains(tasks, "openai.chat") {
		t.Fatalf("tasks = %q", tasks)
	}
	taskID := extract(t, tasks, "")

	// Approve from the terminal; the process resumes and completes.
	if got := run(t, server, "approve", taskID, "-reason", "looks fine"); !strings.Contains(got, "approved") {
		t.Fatalf("approve = %q", got)
	}
	waitStatus(t, server, processID, "completed")
	if got := run(t, server, "proc", processID); !strings.Contains(got, "approved answer") {
		t.Fatalf("proc = %q", got)
	}

	// Second turn on the same session: deny it.
	sent = run(t, server, "send", sessionID, "and again", "-manifest", manifestPath, "-detach")
	processID = extract(t, sent, "process ")
	waitStatus(t, server, processID, "waiting_for_task")
	tasks = run(t, server, "tasks", processID)
	taskID = extract(t, tasks, "")
	if got := run(t, server, "deny", taskID, "-reason", "not today"); !strings.Contains(got, "denied") {
		t.Fatalf("deny = %q", got)
	}
	// The denial fails the cognition syscall; the guest aborts and the
	// process finishes failed, with the reason on the journal.
	waitStatus(t, server, processID, "failed")
	journal := run(t, server, "journal", processID)
	if !strings.Contains(journal, "denied") || !strings.Contains(journal, "not today") {
		t.Fatalf("journal after deny:\n%s", journal)
	}
}

// waitStatus polls the process until it reaches the wanted status.
func waitStatus(t *testing.T, server, processID, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		got := run(t, server, "proc", processID)
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
