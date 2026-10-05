package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestSessionName(t *testing.T) {
	first := sessionName("/home/alex/projects/My App")
	if first != sessionName("/home/alex/projects/My App") {
		t.Fatal("session name must be deterministic")
	}
	if first == sessionName("/other/projects/My App") {
		t.Fatal("different paths must have different session names")
	}
	if !regexp.MustCompile(`^2mux-my-app-[0-9a-f]{10}$`).MatchString(first) {
		t.Fatalf("unexpected session name %q", first)
	}
	if strings.ContainsAny(sessionName("/😀"), ":./ ") {
		t.Fatal("session name contains unsafe characters")
	}
}

func TestRunRejectsInvalidArgumentsBeforeStartingTmux(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"start", "--bogus"}, {"send", "reviewer"},
		{"send", "wrong", "hello"}, {"prompt", "wrong"}, {"status", "extra"},
		{"resolve", "id", "wrong"}, {"_bridge", "missing"},
		{"send", "--queue"}, {"send", "--from", "wrong", "worker", "hello"},
	} {
		if err := run(args); err == nil {
			t.Errorf("accepted invalid arguments %v", args)
		}
	}
}

func TestExplicitQueueWorksWithoutTmuxEnvironment(t *testing.T) {
	dir := queueDir(t)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("PATH", t.TempDir())
	if err := run([]string{"send", "--queue", dir, "--from", "worker", "reviewer", "feedback from a sandboxed agent"}); err != nil {
		t.Fatal(err)
	}
	messages, err := readMessages(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].From != "worker" || messages[0].To != "reviewer" {
		t.Fatalf("wrong message: %+v", messages)
	}
}

func TestSendRejectsEmptyOptionsBeforeSessionDiscovery(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, option := range []string{"--queue", "--from"} {
		err := run([]string{"send", option, "", "reviewer", "feedback"})
		if err == nil || !strings.Contains(err.Error(), option+" requires a non-empty value") {
			t.Errorf("empty %s did not fail before tmux discovery: %v", option, err)
		}
	}
}

func TestExplicitQueueTypedReviewWithoutTmuxEnvironment(t *testing.T) {
	dir := queueDir(t)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("PATH", t.TempDir())
	if err := run([]string{"send", "--queue", dir, "--from", roleWorker, "--kind", "review_request", roleReviewer, "READY_FOR_REVIEW: check the documentation"}); err != nil {
		t.Fatal(err)
	}
	messages, err := readMessages(dir)
	if err != nil || len(messages) != 1 {
		t.Fatalf("request records: %+v, %v", messages, err)
	}
	request := messages[0]
	if request.Kind != "review_request" || request.From != roleWorker || request.To != roleReviewer {
		t.Fatalf("wrong request: %+v", request)
	}
	if err := run([]string{"send", "--queue", dir, "--from", roleReviewer, "--kind", "verdict", "--reply-to", request.ID, "--verdict", "APPROVED", roleWorker, "APPROVED: checked the documentation"}); err != nil {
		t.Fatal(err)
	}
	messages, err = readMessages(dir)
	if err != nil || len(messages) != 2 {
		t.Fatalf("review records: %+v, %v", messages, err)
	}
	verdict := messages[1]
	if verdict.Kind != "verdict" || verdict.ReplyTo != request.ID || verdict.Verdict != "APPROVED" || verdict.From != roleReviewer || verdict.To != roleWorker {
		t.Fatalf("wrong verdict: %+v", verdict)
	}
}
