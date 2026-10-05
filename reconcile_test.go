package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCodexReconnectReconcilesWithoutResubmitting(t *testing.T) {
	dir := queueDir(t)
	messages := []message{}
	for _, text := range []string{"in queue", "in history", "absent"} {
		m := queued(t, dir, roleWorker, text)
		m.Status, m.Transport, m.Attempt = statusSubmitted, "codex", 1
		if err := writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
	}
	socket := fakeCodex(t, func(c *websocket.Conn, e rpcEnvelope) {
		switch e.Method {
		case "thread/queue/list":
			writeRPC(c, map[string]any{"id": e.ID, "result": map[string]any{"data": []any{map[string]string{"clientUserMessageId": messages[0].ID}}}})
		case "thread/items/list":
			writeRPC(c, map[string]any{"id": e.ID, "result": map[string]any{"data": []any{map[string]any{"item": map[string]string{"type": "userMessage", "clientId": messages[1].ID}}}}})
		default:
			t.Errorf("unexpected RPC during reconcile: %s", e.Method)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rpc, err := dialCodex(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.close()
	transport := &nativeTransport{dir: dir, cfg: sessionConfig{CodexThread: "thread"}, rpc: rpc}
	if err := transport.reconcile(); err != nil {
		t.Fatal(err)
	}
	got, err := readMessages(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []messageStatus{statusSubmitted, statusAccepted, statusUncertain} {
		if got[i].Status != want || got[i].Attempt != 1 || got[i].Transport != "codex" {
			t.Fatalf("%+v", got[i])
		}
	}
}

func TestChannelRestartRequiresAckAndBlocksLaterMessages(t *testing.T) {
	dir := queueDir(t)
	m := queued(t, dir, roleReviewer, "unacknowledged")
	m.Status, m.Transport, m.Attempt = statusSubmitted, "claude-channel", 1
	writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m)
	queued(t, dir, roleReviewer, "later")
	if err := reconcileChannel(dir, roleReviewer); err != nil {
		t.Fatal(err)
	}
	if err := deliverQueueTransport(dir, nil, func(message) error { t.Fatal("replayed or delivered past uncertainty"); return nil }, func(message) (string, error) { return "claude-channel", nil }); err == nil {
		t.Fatal("uncertainty hidden")
	}
	if err := acceptMessage(dir, m.ID, roleReviewer); err != nil {
		t.Fatal(err)
	}
	got, _ := readMessages(dir)
	if got[0].Status != statusAccepted || got[1].Status != statusQueued {
		t.Fatal(got)
	}
}

func TestHookCanAcknowledgeLegacyTerminalSubmission(t *testing.T) {
	dir := queueDir(t)
	m := queued(t, dir, roleReviewer, "legacy")
	m.Status, m.Transport = statusDelivered, "tmux"
	writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m)
	payload, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "prompt": formatMessage(m)})
	if err := runHook(dir, roleReviewer, strings.NewReader(string(payload))); err != nil {
		t.Fatal(err)
	}
	got, _ := readMessages(dir)
	if got[0].Status != statusAccepted {
		t.Fatal(got)
	}
	if readAgentState(dir, roleReviewer).State != "busy" {
		t.Fatal("legacy ack lost busy state")
	}
}

func TestChannelReceiptTimeoutIsVisibleWithoutResending(t *testing.T) {
	dir := queueDir(t)
	m := queued(t, dir, roleReviewer, "silent channel")
	m.Status, m.Transport, m.Attempt = statusSubmitted, "claude-channel", 1
	m.SubmittedAt = time.Now().Add(-channelAckTimeout - time.Second)
	writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m)
	if err := expireChannelReceipts(dir, roleReviewer, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := readMessages(dir)
	if got[0].Status != statusUncertain || got[0].Error == "" || got[0].Attempt != 1 {
		t.Fatal(got)
	}
	if err := acceptMessage(dir, m.ID, roleReviewer); err != nil {
		t.Fatal(err)
	}
}
