package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func fakeCodex(t *testing.T, handler func(*websocket.Conn, rpcEnvelope)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "2mux-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var e rpcEnvelope
			if json.Unmarshal(b, &e) != nil {
				return
			}
			if e.Method == "initialize" {
				writeRPC(c, map[string]any{"id": e.ID, "result": map[string]any{}})
			} else if e.Method != "initialized" {
				handler(c, e)
			}
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return socket
}
func writeRPC(c *websocket.Conn, v any) {
	data, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.Write(ctx, websocket.MessageText, data)
}
func TestCodexRPCMultiplexesRepliesAndNeverAnswersApprovals(t *testing.T) {
	var mu sync.Mutex
	var requests []rpcEnvelope
	socket := fakeCodex(t, func(c *websocket.Conn, e rpcEnvelope) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, e)
		if len(requests) == 2 {
			writeRPC(c, map[string]any{"id": "approval", "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": "thread"}})
			for i := 1; i >= 0; i-- {
				writeRPC(c, map[string]any{"id": requests[i].ID, "result": map[string]string{"method": requests[i].Method}})
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := dialCodex(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	var wg sync.WaitGroup
	for _, method := range []string{"first", "second"} {
		method := method
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out map[string]string
			if err := r.call(ctx, method, map[string]any{}, &out); err != nil {
				t.Error(err)
			} else if out["method"] != method {
				t.Error("reply misrouted")
			}
		}()
	}
	wg.Wait()
	select {
	case e := <-r.events:
		if e.Method != "item/commandExecution/requestApproval" {
			t.Fatal(e.Method)
		}
		dir := queueDir(t)
		tr := &nativeTransport{dir: dir, cfg: sessionConfig{CodexThread: "thread"}}
		tr.event(e)
		if readAgentState(dir, roleWorker).State != "awaiting_approval" {
			t.Fatal("server request did not set approval state")
		}
	case <-ctx.Done():
		t.Fatal("event lost")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatal("approval answered by bridge")
	}
}
func TestCodexRPCLostConnectionIsReported(t *testing.T) {
	socket := fakeCodex(t, func(c *websocket.Conn, e rpcEnvelope) { c.CloseNow() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := dialCodex(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if r.call(ctx, "thread/queue/add", map[string]any{}, nil) == nil {
		t.Fatal("lost connection hidden")
	}
}
func TestCodexEventsUseClientIDAndKeepApprovalState(t *testing.T) {
	dir := queueDir(t)
	m := queued(t, dir, "worker", "test")
	m.Transport = "codex"
	m.Status = statusSubmitted
	writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m)
	tr := &nativeTransport{dir: dir, cfg: sessionConfig{CodexThread: "thread"}}
	send := func(method string, p any) {
		data, _ := json.Marshal(p)
		tr.event(rpcEnvelope{Method: method, Params: data})
	}
	send("item/started", map[string]any{"threadId": "other", "item": map[string]string{"type": "userMessage", "clientId": m.ID}})
	msgs, _ := readMessages(dir)
	if msgs[0].Status != statusSubmitted {
		t.Fatal("foreign thread affected receipt")
	}
	send("item/started", map[string]any{"threadId": "thread", "item": map[string]string{"type": "userMessage", "clientId": m.ID}})
	msgs, _ = readMessages(dir)
	if msgs[0].Status != statusAccepted {
		t.Fatal("clientId not acknowledged")
	}
	send("thread/status/changed", map[string]any{"threadId": "thread", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}}})
	if readAgentState(dir, "worker").State != "awaiting_approval" {
		t.Fatal("approval state lost")
	}
	send("turn/completed", map[string]any{"threadId": "thread", "turn": map[string]string{"status": "completed"}})
	msgs, _ = readMessages(dir)
	if msgs[0].Verdict != "" {
		t.Fatal("completion created verdict")
	}
}

// fakeThreads mimics Codex 0.160: a thread is resumable only after it was
// persisted, which thread/start alone does not do.
type fakeThreads struct {
	mu        sync.Mutex
	persisted map[string]bool
	started   []map[string]any
	resumes   []map[string]any
	resumeErr *rpcError
	startErr  *rpcError
	nameErr   *rpcError
	resumed   *codexThread
}

func (f *fakeThreads) handle(c *websocket.Conn, e rpcEnvelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p map[string]any
	json.Unmarshal(e.Params, &p)
	id, _ := p["threadId"].(string)
	switch e.Method {
	case "thread/start":
		f.started = append(f.started, p)
		if f.startErr != nil {
			writeRPC(c, map[string]any{"id": e.ID, "error": f.startErr})
			return
		}
		id = fmt.Sprintf("new-%d", len(f.started))
		writeRPC(c, map[string]any{"id": e.ID, "result": map[string]any{"thread": codexThread{ID: id, CWD: p["cwd"].(string)}}})
	case "thread/name/set":
		if f.nameErr != nil {
			writeRPC(c, map[string]any{"id": e.ID, "error": f.nameErr})
			return
		}
		f.persisted[id] = true
		writeRPC(c, map[string]any{"id": e.ID, "result": map[string]any{}})
	case "thread/resume":
		f.resumes = append(f.resumes, p)
		if f.resumeErr != nil {
			writeRPC(c, map[string]any{"id": e.ID, "error": f.resumeErr})
		} else if !f.persisted[id] {
			writeRPC(c, map[string]any{"id": e.ID, "error": rpcError{Code: -32600, Message: "no rollout found for thread id " + id}})
		} else {
			thread := codexThread{ID: id, CWD: "/project"}
			if f.resumed != nil {
				thread = *f.resumed
			}
			writeRPC(c, map[string]any{"id": e.ID, "result": map[string]any{"thread": thread}})
		}
	}
}

func dialFakeThreads(t *testing.T, f *fakeThreads) (*codexRPC, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	r, err := dialCodex(ctx, fakeCodex(t, f.handle))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	return r, ctx
}

func rolePromptStub() (string, error) { return "role", nil }

func TestEnsureWorkerThreadPersistsNewThreadForResume(t *testing.T) {
	f := &fakeThreads{persisted: map[string]bool{}}
	r, ctx := dialFakeThreads(t, f)
	dir := queueDir(t)
	cfg := sessionConfig{CWD: "/project"}
	thread, err := ensureWorkerThread(ctx, r, dir, &cfg, rolePromptStub)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if !f.persisted[thread.ID] {
		t.Fatal("new thread was not persisted; the TUI could not resume it")
	}
	if len(f.started[0]) != 2 || f.started[0]["cwd"] != "/project" || f.started[0]["developerInstructions"] != "role" {
		t.Fatalf("thread/start params: %v", f.started[0])
	}
	f.mu.Unlock()
	again, err := ensureWorkerThread(ctx, r, dir, &cfg, rolePromptStub)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || again.ID != thread.ID || len(f.started) != 1 {
		t.Fatalf("persisted thread not reused: %q %v starts=%d", again, err, len(f.started))
	}
	saved, err := readSessionConfig(dir)
	if err != nil || saved.CodexThread != thread.ID {
		t.Fatalf("replacement not saved: %+v: %v", saved, err)
	}
}

func TestEnsureWorkerThreadReplacesUnpersistedThread(t *testing.T) {
	f := &fakeThreads{persisted: map[string]bool{}}
	r, ctx := dialFakeThreads(t, f)
	dir := queueDir(t)
	cfg := sessionConfig{CWD: "/project", CodexThread: "stale"}
	thread, err := ensureWorkerThread(ctx, r, dir, &cfg, rolePromptStub)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if thread.ID == "stale" || !f.persisted[thread.ID] || len(f.started) != 1 {
		t.Fatalf("stale thread kept or replacement not persisted: %q", thread.ID)
	}
}

func TestEnsureWorkerThreadKeepsThreadOnOtherResumeErrors(t *testing.T) {
	f := &fakeThreads{persisted: map[string]bool{}, resumeErr: &rpcError{Code: -32603, Message: "backend busy"}}
	r, ctx := dialFakeThreads(t, f)
	cfg := sessionConfig{CWD: "/project", CodexThread: "thread"}
	if _, err := ensureWorkerThread(ctx, r, queueDir(t), &cfg, rolePromptStub); err == nil || missingRollout(err) {
		t.Fatalf("transient resume error hidden or misclassified: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.started) != 0 || cfg.CodexThread != "thread" {
		t.Fatal("a transient resume error replaced the worker thread")
	}
}

func TestMissingRolloutRequiresExactRPCError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&rpcError{Code: -32600, Message: "no rollout found for thread id stale"}, true},
		{fmt.Errorf("wrapped: %w", &rpcError{Code: -32600, Message: "no rollout found for thread id stale"}), true},
		{&rpcError{Code: -32603, Message: "no rollout found"}, false},
		{&rpcError{Code: -32600, Message: "thread cwd mismatch"}, false},
		{errors.New("no rollout found"), false},
	} {
		if got := missingRollout(tc.err); got != tc.want {
			t.Fatalf("missingRollout(%v) = %v", tc.err, got)
		}
	}
}

func TestWorkerThreadInvalidationSurvivesReplacementFailure(t *testing.T) {
	for _, failure := range []string{"start", "name"} {
		t.Run(failure, func(t *testing.T) {
			f := &fakeThreads{persisted: map[string]bool{}}
			switch failure {
			case "start":
				f.startErr = &rpcError{Code: -32603, Message: "start failed"}
			case "name":
				f.nameErr = &rpcError{Code: -32603, Message: "persistence failed"}
			}
			r, ctx := dialFakeThreads(t, f)
			dir := queueDir(t)
			cfg := sessionConfig{CWD: "/project", CodexAPI: true, CodexThread: "stale", ClaudeSession: "keep", BackendPane: "%3"}
			if err := writeSessionConfig(dir, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := ensureWorkerThread(ctx, r, dir, &cfg, rolePromptStub); err == nil {
				t.Fatal("replacement failure hidden")
			}
			saved, err := readSessionConfig(dir)
			if err != nil || saved.CodexThread != "" || !saved.CodexAPI || saved.ClaudeSession != "keep" || saved.BackendPane != "%3" {
				t.Fatalf("invalidation lost or config damaged: %+v: %v", saved, err)
			}
		})
	}
}

func TestWorkerThreadIdentityMismatchDoesNotReplaceOrChangeConfig(t *testing.T) {
	for _, thread := range []codexThread{{ID: "foreign", CWD: "/project"}, {ID: "thread", CWD: "/other"}} {
		t.Run(thread.ID+thread.CWD, func(t *testing.T) {
			f := &fakeThreads{persisted: map[string]bool{"thread": true}, resumed: &thread}
			r, ctx := dialFakeThreads(t, f)
			dir := queueDir(t)
			cfg := sessionConfig{CWD: "/project", CodexThread: "thread"}
			if err := writeSessionConfig(dir, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := ensureWorkerThread(ctx, r, dir, &cfg, rolePromptStub); err == nil {
				t.Fatal("identity mismatch accepted")
			}
			saved, err := readSessionConfig(dir)
			f.mu.Lock()
			defer f.mu.Unlock()
			if err != nil || saved != cfg || cfg.CodexThread != "thread" || len(f.started) != 0 {
				t.Fatalf("identity mismatch changed thread/config: %+v: %v", saved, err)
			}
		})
	}
}

func TestWorkerThreadResumesCarryRoleInstructions(t *testing.T) {
	f := &fakeThreads{persisted: map[string]bool{}}
	r, ctx := dialFakeThreads(t, f)
	dir := queueDir(t)
	cfg := sessionConfig{CWD: "/project"}
	thread, err := ensureWorkerThread(ctx, r, dir, &cfg, rolePromptStub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureWorkerThread(ctx, r, dir, &cfg, rolePromptStub); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// The first resume persists the new thread on its creating connection; the
	// second is a later restart. Both must restore the role instructions, which
	// Codex does not keep in the rollout.
	if len(f.resumes) != 2 {
		t.Fatalf("resumes: %v", f.resumes)
	}
	for _, p := range f.resumes {
		if p["threadId"] != thread.ID || p["developerInstructions"] != "role" {
			t.Fatalf("resume without role instructions: %v", p)
		}
	}
}

func TestWorkerThreadPromptFailureKeepsConfig(t *testing.T) {
	f := &fakeThreads{persisted: map[string]bool{}}
	r, ctx := dialFakeThreads(t, f)
	dir := queueDir(t)
	cfg := sessionConfig{CWD: "/project", CodexAPI: true, CodexThread: "stale"}
	if err := writeSessionConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	failing := func() (string, error) { return "", errors.New("prompt failed") }
	if _, err := ensureWorkerThread(ctx, r, dir, &cfg, failing); err == nil {
		t.Fatal("prompt failure hidden")
	}
	saved, err := readSessionConfig(dir)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || saved.CodexThread != "stale" || len(f.resumes) != 0 || len(f.started) != 0 {
		t.Fatalf("prompt failure touched thread or config: %+v %v", saved, err)
	}
}
