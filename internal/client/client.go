// Package client is the aurora-dist /v1 API client. It deliberately defines
// its own wire types instead of importing the runtime: the CLI is the API's
// first external consumer, and a client that borrowed the server's structs
// would hide wire gaps instead of exposing them.
package client

import (
	"bufio"
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

type Session struct {
	SessionSummary
	History   []Message `json:"history"`
	Processes []Process `json:"processes"`
}

type Process struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"session_id"`
	Message       string          `json:"message"`
	Status        string          `json:"status"`
	Attempt       int             `json:"attempt"`
	Revision      uint64          `json:"revision"`
	Answer        string          `json:"answer,omitempty"`
	Error         string          `json:"error,omitempty"`
	JournalLength int             `json:"journal_length"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	ProgramDigest string          `json:"program_digest"`
	Manifest      json.RawMessage `json:"manifest,omitempty"`
}

// Terminal reports whether the process reached a final state.
func (p Process) Terminal() bool {
	switch p.Status {
	case "completed", "failed", "stopped", "interrupted":
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

type Program struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

type Retention struct {
	Digest           string   `json:"digest"`
	Programs         []string `json:"programs,omitempty"`
	Processes        []string `json:"processes,omitempty"`
	Decommissionable bool     `json:"decommissionable"`
}

// Event is one SSE event: Type from the event field, ID from the id field
// (the firehose resume cursor), Data verbatim.
type Event struct {
	Type string
	ID   string
	Data json.RawMessage
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
		return fmt.Errorf("%s %s: %s (%s)", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	var out []SessionSummary
	err := c.do(ctx, http.MethodGet, "/v1/sessions", nil, &out)
	return out, err
}

func (c *Client) CreateSession(ctx context.Context, tags map[string]string) (Session, error) {
	var out Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions", map[string]any{"tags": tags}, &out)
	return out, err
}

func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	var out Session
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) CreateProcess(ctx context.Context, sessionID, message string, manifest json.RawMessage) (Process, error) {
	body := map[string]any{"message": message}
	if len(manifest) > 0 {
		body["manifest"] = manifest
	}
	var out Process
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/processes", body, &out)
	return out, err
}

func (c *Client) GetProcess(ctx context.Context, id string) (Process, error) {
	var out Process
	err := c.do(ctx, http.MethodGet, "/v1/processes/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) Journal(ctx context.Context, processID string) ([]JournalEntry, error) {
	var out []JournalEntry
	err := c.do(ctx, http.MethodGet, "/v1/processes/"+url.PathEscape(processID)+"/journal", nil, &out)
	return out, err
}

func (c *Client) JournalRevisions(ctx context.Context, processID string) (map[uint64][]JournalEntry, error) {
	var out map[uint64][]JournalEntry
	err := c.do(ctx, http.MethodGet, "/v1/processes/"+url.PathEscape(processID)+"/journal/revisions", nil, &out)
	return out, err
}

func (c *Client) Tasks(ctx context.Context, processID string) ([]Task, error) {
	var out []Task
	err := c.do(ctx, http.MethodGet, "/v1/processes/"+url.PathEscape(processID)+"/tasks", nil, &out)
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

func (c *Client) Programs(ctx context.Context) ([]Program, error) {
	var out []Program
	err := c.do(ctx, http.MethodGet, "/v1/programs", nil, &out)
	return out, err
}

func (c *Client) ReloadPrograms(ctx context.Context) ([]Program, error) {
	var out []Program
	err := c.do(ctx, http.MethodPost, "/v1/programs/reload", map[string]any{}, &out)
	return out, err
}

func (c *Client) Retention(ctx context.Context) ([]Retention, error) {
	var out []Retention
	err := c.do(ctx, http.MethodGet, "/v1/programs/retention", nil, &out)
	return out, err
}

// SessionEvents streams one session's SSE feed (snapshot first, then live)
// until ctx is done or the server closes the stream.
func (c *Client) SessionEvents(ctx context.Context, sessionID string) (<-chan Event, error) {
	return c.stream(ctx, "/v1/sessions/"+url.PathEscape(sessionID)+"/events")
}

// Firehose streams the tenant-wide event feed. after > 0 resumes from that
// cursor (replay from the ring, or a fresh snapshot when scrolled out).
func (c *Client) Firehose(ctx context.Context, after uint64) (<-chan Event, error) {
	path := "/v1/events"
	if after > 0 {
		path = fmt.Sprintf("%s?after=%d", path, after)
	}
	return c.stream(ctx, path)
}

func (c *Client) stream(ctx context.Context, path string) (<-chan Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s (%s)", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	events := make(chan Event, 32)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		var event Event
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Type = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				event.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				event.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			case line == "":
				if event.Type != "" || event.Data != nil {
					select {
					case events <- event:
					case <-ctx.Done():
						return
					}
				}
				event = Event{}
			}
		}
	}()
	return events, nil
}
