// Package client is the aurora-dist /v1 API client. It deliberately defines
// its own wire types instead of importing the runtime: the CLI is the API's
// first external consumer, and a client that borrowed the server's structs
// would hide wire gaps instead of exposing them.
//
// The read surface is deliberately small. GET /v1/sessions/{id} returns the
// whole session log — session metadata, history, and every process with its
// full state, delegation links, journal across all revisions, and tasks — and
// every narrower view (the current journal, one revision, the call graph, a
// task list) is a grouping of that one payload, computed here in the terminal.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// --- wire types (/v1) ---

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type SessionSummary struct {
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	ProcessCount    int               `json:"process_count"`
	ActiveProcessID string            `json:"active_process_id,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
}

// SessionLog is the one comprehensive read: GET /v1/sessions/{id}.
type SessionLog struct {
	Session   SessionSummary `json:"session"`
	History   []Message      `json:"history,omitempty"`
	Processes []ProcessLog   `json:"processes"`
}

type Process struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"session_id"`
	Input         string          `json:"input"`
	Status        string          `json:"status"`
	Attempt       int             `json:"attempt"`
	Revision      uint64          `json:"revision"`
	Answer        string          `json:"answer,omitempty"`
	Error         string          `json:"error,omitempty"`
	JournalLength int             `json:"journal_length"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	StartedAt     *time.Time      `json:"started_at,omitempty"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
	ProgramDigest string          `json:"program_digest"`
	Manifest      json.RawMessage `json:"manifest,omitempty"`
}

// ProcessLog is a process within a session log: its snapshot plus delegation
// links, its complete journal across all revisions, and its tasks.
type ProcessLog struct {
	Process
	ParentProcessID string         `json:"parent_process_id,omitempty"`
	ChildProcessIDs []string       `json:"child_process_ids,omitempty"`
	Entries         []JournalEntry `json:"entries"`
	Tasks           []Task         `json:"tasks,omitempty"`
}

// Terminal reports whether the process reached a final state.
func (p Process) Terminal() bool {
	switch p.Status {
	case "completed", "failed", "stopped", "interrupted", "compensated":
		return true
	}
	return false
}

// Parked reports whether the process is durably suspended awaiting
// out-of-band resolution.
func (p Process) Parked() bool {
	return p.Status == "waiting_for_task" || p.Status == "yielded"
}

type Syscall struct {
	Abi  int             `json:"abi"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type Outcome struct {
	Status  string          `json:"status"`
	Code    string          `json:"code,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Message string          `json:"message,omitempty"`
	Labels  []string        `json:"labels,omitempty"`
}

type JournalEntry struct {
	Position    int     `json:"position"`
	Revision    uint64  `json:"revision"`
	Syscall     Syscall `json:"syscall"`
	Outcome     Outcome `json:"outcome"`
	Compensates *int    `json:"compensates,omitempty"`
}

// Program is one loaded program artifact, with the interface it bundles: what
// its input message and answer look like (JSON Schemas), so a caller knows what
// to pass and what comes back without reading the wasm.
type Program struct {
	ID          string          `json:"id"`
	Digest      string          `json:"digest"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	Output      json.RawMessage `json:"output"`
}

type Resolution struct {
	Decision string          `json:"decision,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Actor    string          `json:"actor,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}

type Task struct {
	ID              string     `json:"id"`
	ProcessID       string     `json:"process_id"`
	Revision        uint64     `json:"revision"`
	JournalPosition int        `json:"journal_position"`
	Syscall         Syscall    `json:"syscall"`
	Summary         string     `json:"summary"`
	State           string     `json:"state"`
	Resolution      Resolution `json:"resolution,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	ResolutionToken string     `json:"resolution_token"`
}

// --- client ---

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: http.DefaultClient}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var body struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(raw, &body) == nil && body.Error != "" {
			return &APIError{Status: resp.StatusCode, Code: body.Code, Message: body.Error}
		}
		return fmt.Errorf("%s %s: %s (%s)", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// APIError is a structured {error, code} failure from the /v1 API. Callers can
// branch on Code (e.g. "conflict", "not_found") instead of matching prose.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

func (c *Client) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	var out []SessionSummary
	err := c.do(ctx, http.MethodGet, "/v1/sessions", nil, &out)
	return out, err
}

// Programs lists the loaded program artifacts (read-only; the set is
// reconciled from the server's programs directory).
func (c *Client) Programs(ctx context.Context) ([]Program, error) {
	var out []Program
	err := c.do(ctx, http.MethodGet, "/v1/programs", nil, &out)
	return out, err
}

func (c *Client) CreateSession(ctx context.Context, tags map[string]string) (SessionLog, error) {
	var out SessionLog
	err := c.do(ctx, http.MethodPost, "/v1/sessions", map[string]any{"tags": tags}, &out)
	return out, err
}

// Session returns the complete session log — the one comprehensive read.
func (c *Client) Session(ctx context.Context, id string) (SessionLog, error) {
	var out SessionLog
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) CreateProcess(ctx context.Context, sessionID, input string, manifest json.RawMessage) (Process, error) {
	body := map[string]any{"input": input}
	if len(manifest) > 0 {
		body["manifest"] = manifest
	}
	var out Process
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/processes", body, &out)
	return out, err
}

// GetProcess is the cheap single-process status poll.
func (c *Client) GetProcess(ctx context.Context, id string) (Process, error) {
	var out Process
	err := c.do(ctx, http.MethodGet, "/v1/processes/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) ResolveTask(ctx context.Context, taskID, token string, resolution Resolution) (Task, error) {
	var out Task
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/resolve", map[string]any{
		"resolution_token": token,
		"resolution":       resolution,
	}, &out)
	return out, err
}

func (c *Client) Stop(ctx context.Context, processID string) (Process, error) {
	var out Process
	err := c.do(ctx, http.MethodPost, "/v1/processes/"+url.PathEscape(processID)+"/stop", map[string]any{}, &out)
	return out, err
}

func (c *Client) Retry(ctx context.Context, processID, mode string) (Process, error) {
	var out Process
	err := c.do(ctx, http.MethodPost, "/v1/processes/"+url.PathEscape(processID)+"/retry", map[string]any{"mode": mode}, &out)
	return out, err
}
