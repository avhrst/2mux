package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func queueDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "messages"), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func queued(t *testing.T, dir, to, text string) message {
	t.Helper()
	m, err := enqueue(dir, "worker", to, text)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestQueueBidirectionalAndNoReplay(t *testing.T) {
	dir := queueDir(t)
	first := queued(t, dir, "reviewer", "READY_FOR_REVIEW\nChanged bridge.go; tests pass.")
	second := queued(t, dir, "worker", "CORRECTIONS\nFix the queue.\nПеревір UTF-8.")
	var got []message
	deliver := func(m message) error { got = append(got, m); return nil }
	for i := 0; i < 3; i++ {
		if err := deliverQueue(dir, deliver); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID || got[1].Text != second.Text {
		t.Fatalf("unexpected delivery: %+v", got)
	}
	messages, err := readMessages(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Status != "delivered" {
			t.Fatalf("unexpected status %s", m.Status)
		}
	}
}

func TestQueueWaitsForRecipientAndPreservesOrder(t *testing.T) {
	dir := queueDir(t)
	first := queued(t, dir, "reviewer", "first")
	queued(t, dir, "reviewer", "second")
	other := queued(t, dir, "worker", "other direction")
	var got []string
	err := deliverQueue(dir, func(m message) error {
		got = append(got, m.ID)
		if m.To == "reviewer" {
			return errors.New("agent not running")
		}
		return nil
	})
	if err == nil || len(got) != 2 || got[0] != first.ID || got[1] != other.ID {
		t.Fatalf("unexpected attempts: %v, %v", got, err)
	}
	var retried []string
	if err := deliverQueue(dir, func(m message) error { retried = append(retried, m.Text); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(retried, ",") != "first,second" {
		t.Fatalf("out of order: %v", retried)
	}
}

func TestUncertainDeliveryIsNotRetriedAndBlocksRecipient(t *testing.T) {
	dir := queueDir(t)
	first := queued(t, dir, "reviewer", "first")
	queued(t, dir, "reviewer", "second")
	count := 0
	deliver := func(m message) error { count++; return uncertainDelivery{errors.New("submission outcome unknown")} }
	_ = deliverQueue(dir, deliver)
	_ = deliverQueue(dir, deliver)
	if count != 1 {
		t.Fatalf("uncertain delivery was retried %d times", count)
	}
	if err := resolveMessage(dir, first.ID, "delivered"); err != nil {
		t.Fatal(err)
	}
	if err := deliverQueue(dir, func(m message) error {
		if m.Text != "second" {
			t.Fatal(m.Text)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCrashDuringDeliveryRequiresResolution(t *testing.T) {
	dir := queueDir(t)
	m := queued(t, dir, "reviewer", "message")
	m.Status = "sending"
	if err := writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m); err != nil {
		t.Fatal(err)
	}
	if err := deliverQueue(dir, func(message) error { t.Fatal("interrupted send must not replay"); return nil }); err == nil {
		t.Fatal("missing uncertain error")
	}
	if err := resolveMessage(dir, m.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	if err := deliverQueue(dir, func(message) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentEnqueue(t *testing.T) {
	dir := queueDir(t)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := enqueue(dir, "worker", "reviewer", "feedback"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	count := 0
	for i := 0; i < 4; i++ {
		if err := deliverQueue(dir, func(message) error { count++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if count != 30 {
		t.Fatalf("lost messages: %d", count)
	}
}

func TestQueueRejectsInvalidInput(t *testing.T) {
	dir := queueDir(t)
	for _, text := range []string{"", " \n", "hi\x1b[2J", "hi\rcommand", "\xff", strings.Repeat("a", maxMessageBytes+1)} {
		if _, err := enqueue(dir, "worker", "reviewer", text); err == nil {
			t.Errorf("accepted invalid input %q", text[:min(len(text), 20)])
		}
	}
	if _, err := enqueue(dir, "worker", "unknown", "hi"); err == nil {
		t.Fatal("accepted invalid role")
	}
	for _, sender := range []string{"", "unknown", "worker\nINJECTED", "worker\x1b[2J"} {
		if _, err := enqueue(dir, sender, "reviewer", "hi"); err == nil {
			t.Errorf("accepted invalid sender %q", sender)
		}
	}
}

func TestCorruptMessageMetadataIsNotDelivered(t *testing.T) {
	for name, corrupt := range map[string]func(*message){
		"missing sender":    func(m *message) { m.From = "" },
		"unknown sender":    func(m *message) { m.From = "other" },
		"header injection":  func(m *message) { m.From = "worker\x1b[2J" },
		"missing timestamp": func(m *message) { m.Created = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			dir := queueDir(t)
			m := queued(t, dir, "reviewer", "valid body")
			corrupt(&m)
			if err := writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m); err != nil {
				t.Fatal(err)
			}
			called := false
			err := deliverQueue(dir, func(message) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("corrupt metadata reached delivery: called=%v, err=%v", called, err)
			}
		})
	}
}

func TestForegroundAgentDetection(t *testing.T) {
	for _, input := range []string{
		"S+ /usr/bin/zsh zsh\nS+ /usr/bin/codex codex prompt",
		"S+ /opt/node node /opt/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js",
		"S+ /opt/bin/pi pi",
		"Ss+ node pi",
		"S+ bun bun /opt/pi-coding-agent/cli.js",
		"S+ bun bun run /opt/pi-coding-agent/cli.js",
	} {
		if !hasForegroundAgent(input) {
			t.Errorf("did not recognize %q", input)
		}
	}
	for _, input := range []string{
		"S+ /bin/zsh zsh", "S+ node node app.js", "S /bin/codex codex",
		"S+ cat cat codex", "S+ python python pi.py",
		"S+ node node app.js /opt/pi-coding-agent/cli.js",
		"S+ node node --require /opt/pi-coding-agent/cli.js",
		"S+ node node --require=/opt/pi-coding-agent/cli.js app.js",
		"S+ bun bun run app.js /opt/pi-coding-agent/cli.js",
	} {
		if hasForegroundAgent(input) {
			t.Errorf("accepted non-agent %q", input)
		}
	}
}

func TestBridgeLockIsExclusive(t *testing.T) {
	dir := queueDir(t)
	f, ok, err := tryLock(filepath.Join(dir, "bridge.lock"))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	running, err := bridgeRunning(dir)
	if err != nil || !running {
		t.Fatal(running, err)
	}
	f.Close()
	running, err = bridgeRunning(dir)
	if err != nil || running {
		t.Fatal(running, err)
	}
}

func TestForegroundPiSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "pi-coding-agent", "cli.js")
	if err := os.Mkdir(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "pi")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if !hasForegroundAgent("S+ node node " + link) {
		t.Fatal("installed pi symlink not recognized")
	}
}
