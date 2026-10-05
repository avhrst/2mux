package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerdictRequiresExactRequest(t *testing.T) {
	dir := queueDir(t)
	request, err := enqueue(dir, "worker", "reviewer", "review", sendOptions{Kind: "review_request"})
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []sendOptions{{Kind: "verdict", Verdict: "APPROVED"}, {Kind: "verdict", ReplyTo: request.ID, Verdict: "yes"}, {Kind: "verdict", ReplyTo: strings.Repeat("a", 32), Verdict: "APPROVED"}} {
		if _, err := enqueue(dir, "reviewer", "worker", "approved", options); err == nil {
			t.Fatal("invalid verdict accepted")
		}
	}
	if _, err := enqueue(dir, "reviewer", "worker", "APPROVED", sendOptions{Kind: "verdict", ReplyTo: request.ID, Verdict: "APPROVED"}); err != nil {
		t.Fatal(err)
	}
	if _, err := enqueue(dir, "reviewer", "worker", "APPROVED", sendOptions{Kind: "verdict", ReplyTo: request.ID, Verdict: "APPROVED"}); err == nil {
		t.Fatal("duplicate verdict")
	}
}
func TestReviewScopeIncludesUntrackedAndPendingVerdict(t *testing.T) {
	dir := queueDir(t)
	project := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = project
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	git("init", "-q")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "initial")
	if err := writeSessionConfig(dir, sessionConfig{CWD: project}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "untracked.txt")
	os.WriteFile(path, []byte("first"), 0600)
	r, err := enqueue(dir, "worker", "reviewer", "review", sendOptions{Kind: "review_request"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := enqueue(dir, "reviewer", "worker", "APPROVED", sendOptions{Kind: "verdict", ReplyTo: r.ID, Verdict: "APPROVED"})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("second"), 0600)
	if err := validateVerdictScope(dir, v); err == nil {
		t.Fatal("pending stale verdict valid")
	}
	if _, err := enqueue(dir, "reviewer", "worker", "CORRECTIONS", sendOptions{Kind: "verdict", ReplyTo: r.ID, Verdict: "CORRECTIONS"}); err == nil {
		t.Fatal("stale verdict enqueued")
	}
	if err := deliverQueue(dir, nil, func(m message) error {
		if m.ID == v.ID {
			t.Fatal("stale verdict delivered")
		}
		return nil
	}); err == nil {
		t.Fatal("stale verdict error hidden")
	}
	messages, _ := readMessages(dir)
	if messages[1].Status != statusRejected || messages[1].Error == "" {
		t.Fatalf("%+v", messages[1])
	}
	// A rejected verdict is terminal; another tick does not recompute scope.
	if err := deliverQueue(dir, nil, func(message) error { t.Fatal("rejected verdict replayed"); return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestNativeAttemptNeverChangesTransport(t *testing.T) {
	dir := queueDir(t)
	m := queued(t, dir, "worker", "native")
	err := deliverQueueTransport(dir, nil, func(message) error { return uncertainDelivery{os.ErrDeadlineExceeded} }, func(message) (string, error) { return "codex", nil })
	if err == nil {
		t.Fatal("lost submit hidden")
	}
	deliverQueueTransport(dir, nil, func(message) error { t.Fatal("ambiguous message replayed"); return nil }, func(message) (string, error) { return "tmux", nil })
	msgs, _ := readMessages(dir)
	if msgs[0].ID != m.ID || msgs[0].Status != statusUncertain || msgs[0].Transport != "codex" || msgs[0].Attempt != 1 {
		t.Fatalf("%+v", msgs)
	}
}

func TestSteerRequiresAnActiveNativeTurnAndPinsItsID(t *testing.T) {
	dir := queueDir(t)
	writeSessionConfig(dir, sessionConfig{CodexAPI: true})
	if _, err := enqueue(dir, senderUser, roleWorker, "steer", sendOptions{Steer: true}); err == nil {
		t.Fatal("steer without turn enqueued")
	}
	writeAgentState(dir, agentState{Role: roleWorker, State: "busy", Source: "codex-api", TurnID: "active-turn"})
	m, err := enqueue(dir, senderUser, roleWorker, "steer", sendOptions{Steer: true})
	if err != nil || m.TurnRef != "active-turn" {
		t.Fatal(m, err)
	}
}
