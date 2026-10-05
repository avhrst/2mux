package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHooksStateAndPrivacy(t *testing.T) {
	dir := queueDir(t)
	writeSessionConfig(dir, sessionConfig{ClaudeSession: "session"})
	for _, tc := range []struct{ event, notification, want string }{{"SessionStart", "", "starting"}, {"UserPromptSubmit", "", "busy"}, {"Notification", "permission_prompt", "awaiting_approval"}, {"Stop", "", "idle"}, {"SessionEnd", "", "exited"}} {
		payload, _ := json.Marshal(map[string]string{"session_id": "session", "hook_event_name": tc.event, "notification_type": tc.notification, "prompt": "PRIVATE_FIXTURE_TEXT"})
		if err := runHook(dir, "reviewer", strings.NewReader(string(payload))); err != nil {
			t.Fatal(err)
		}
		s := readAgentState(dir, "reviewer")
		if s.State != tc.want {
			t.Fatalf("%s -> %s", tc.event, s.State)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "reviewer-state.json"))
		if strings.Contains(string(data), "PRIVATE_FIXTURE_TEXT") {
			t.Fatal("prompt persisted")
		}
	}
	if err := runHook(dir, "reviewer", strings.NewReader(`{"session_id":"wrong","hook_event_name":"Stop"}`)); err == nil {
		t.Fatal("foreign hook accepted")
	}
}
func TestAcknowledgementDoesNotCreateVerdict(t *testing.T) {
	dir := queueDir(t)
	m := queued(t, dir, "reviewer", "test")
	m.Status = statusSubmitted
	m.Transport = "claude-channel"
	writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m)
	if err := acceptMessage(dir, m.ID, "worker"); err == nil {
		t.Fatal("foreign ack")
	}
	if err := acceptMessage(dir, m.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := readMessages(dir)
	if msgs[0].Status != statusAccepted || msgs[0].Verdict != "" {
		t.Fatal("ack created a review result")
	}
}

func TestHookMissingReceiptStillRecordsBusy(t *testing.T) {
	dir := queueDir(t)
	payload, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "prompt": "[2mux message " + strings.Repeat("a", 32) + " from worker to reviewer]"})
	if err := runHook(dir, roleReviewer, strings.NewReader(string(payload))); err != nil {
		t.Fatal(err)
	}
	if readAgentState(dir, roleReviewer).State != "busy" {
		t.Fatal("ack failure lost busy event")
	}
}
