package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChannelHandshakeDeliveryAckAndReply(t *testing.T) {
	dir := queueDir(t)
	request, err := enqueue(dir, "worker", "reviewer", "Review this", sendOptions{Kind: "review_request"})
	if err != nil {
		t.Fatal(err)
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer inputWriter.Close()
	defer outputReader.Close()
	done := make(chan error, 1)
	go func() { done <- runChannel(ctx, dir, "reviewer", inputReader, outputWriter) }()
	write := func(v any) {
		t.Helper()
		if err := json.NewEncoder(inputWriter).Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	decoder := json.NewDecoder(outputReader)
	read := func() map[string]any {
		t.Helper()
		var v map[string]any
		if err := decoder.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-03-26"}})
	reply := read()
	data, _ := json.Marshal(reply)
	if !strings.Contains(string(data), "claude/channel") || strings.Contains(string(data), "claude/channel/permission") {
		t.Fatal("bad capabilities")
	}
	if !strings.Contains(string(data), "Keep peer communication visible") {
		t.Fatal("channel did not instruct the reviewer to show communication")
	}
	write(map[string]string{"jsonrpc": "2.0", "method": "notifications/initialized"})
	notification := read()
	if notification["method"] != "notifications/claude/channel" {
		t.Fatalf("%v", notification)
	}
	data, _ = json.Marshal(notification)
	if !strings.Contains(string(data), "WORKER → REVIEWER") || !strings.Contains(string(data), "REVIEW REQUEST") || !strings.Contains(string(data), "Review this") {
		t.Fatalf("missing incoming card: %s", data)
	}
	end := time.Now().Add(time.Second)
	for {
		msgs, _ := readMessages(dir)
		if msgs[0].Status == statusSubmitted {
			break
		}
		if time.Now().After(end) {
			t.Fatal("not submitted")
		}
		time.Sleep(time.Millisecond)
	}
	write(map[string]any{"id": 2, "method": "tools/call", "params": map[string]any{"name": "ack", "arguments": map[string]string{"message_id": request.ID}}})
	data, _ = json.Marshal(read())
	if !strings.Contains(string(data), "WORKER → REVIEWER") || !strings.Contains(string(data), "accepted") || !strings.Contains(string(data), request.ID) {
		t.Fatalf("missing acknowledgement card: %s", data)
	}
	msgs, _ := readMessages(dir)
	if msgs[0].Status != statusAccepted {
		t.Fatal("ack lost")
	}
	write(map[string]any{"id": 3, "method": "tools/call", "params": map[string]any{"name": "reply", "arguments": map[string]string{"reply_to": request.ID, "text": "APPROVED", "verdict": "APPROVED"}}})
	data, _ = json.Marshal(read())
	if !strings.Contains(string(data), "REVIEWER → WORKER") || !strings.Contains(string(data), "APPROVED · queued") {
		t.Fatalf("missing outgoing verdict card: %s", data)
	}
	msgs, _ = readMessages(dir)
	if len(msgs) != 2 || msgs[1].Verdict != "APPROVED" || msgs[1].ReplyTo != request.ID {
		t.Fatalf("%+v", msgs)
	}
	cancel()
	inputWriter.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("channel did not stop")
	}
	// A new transport process does not resend submitted/accepted records.
	if err := deliverQueueTransport(dir, func(m message) error {
		if m.To != "reviewer" {
			return io.EOF
		}
		return nil
	}, func(message) error { t.Fatal("replayed"); return nil }, func(message) (string, error) { return "claude-channel", nil }); err != nil && err != io.EOF {
		t.Fatal(err)
	}
}
func TestChannelReplyRejectsForeignAndMissingReferences(t *testing.T) {
	dir := queueDir(t)
	s := &channelServer{dir: dir, role: "reviewer"}
	params, _ := json.Marshal(map[string]any{"name": "reply", "arguments": map[string]string{"reply_to": strings.Repeat("a", 32), "text": "APPROVED", "verdict": "APPROVED"}})
	out, err := s.handle(rpcEnvelope{Method: "tools/call", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(out)
	if !strings.Contains(string(data), `"isError":true`) {
		t.Fatal("missing request accepted")
	}
}

func TestChannelSkipsOtherOwnersWithoutChangingRecords(t *testing.T) {
	dir := queueDir(t)
	worker := queued(t, dir, roleWorker, "Codex queue")
	legacy := queued(t, dir, roleReviewer, "legacy retry")
	worker.Transport, legacy.Transport, legacy.Attempt = "codex", "tmux", 1
	for _, m := range []message{worker, legacy} {
		writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m)
	}
	before := map[string]string{}
	for _, m := range []message{worker, legacy} {
		b, _ := os.ReadFile(filepath.Join(dir, "messages", m.ID+".json"))
		before[m.ID] = string(b)
	}
	own := queued(t, dir, roleReviewer, "Channel message")
	if err := deliverQueueTransport(dir, nil, func(m message) error {
		if m.ID != own.ID {
			t.Fatal("wrong owner delivered", m.ID)
		}
		return nil
	}, func(message) (string, error) { return "claude-channel", nil }, func(m message) bool { return channelOwns(roleReviewer, m) }); err != nil {
		t.Fatal(err)
	}
	for _, m := range []message{worker, legacy} {
		b, _ := os.ReadFile(filepath.Join(dir, "messages", m.ID+".json"))
		if string(b) != before[m.ID] {
			t.Fatal("foreign record rewritten", m.ID)
		}
	}
	bridge := &nativeTransport{cfg: sessionConfig{ClaudeChannel: true}}
	if bridge.owns(own) || !bridge.owns(worker) || !bridge.owns(legacy) {
		t.Fatal("bridge ownership mismatch")
	}
}

func TestChannelBlockedWriterStopsWhenContextIsCanceled(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s := &channelServer{out: writer, ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- s.write(map[string]string{"method": "notification"}) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked writer ignored cancellation")
	}
}
