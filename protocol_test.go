package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstalledCodexSchemaContract(t *testing.T) {
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil || !supportedCodexVersion(strings.TrimSpace(string(version))) {
		t.Skip("Codex version outside the native transport gate")
	}
	dir := t.TempDir()
	if output, err := exec.CommandContext(ctx, path, "app-server", "generate-json-schema", "--experimental", "--out", dir).CombinedOutput(); err != nil {
		t.Fatalf("generate schema: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "codex_app_server_protocol.schemas.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"initialize", "thread/start", "thread/resume", "thread/name/set", "thread/queue/add", "thread/queue/list", "thread/items/list", "turn/steer"} {
		if !strings.Contains(string(data), `"`+method+`"`) {
			t.Errorf("missing RPC method %s", method)
		}
	}
	for file, field := range map[string]string{"v2/ThreadQueueAddParams.json": "clientUserMessageId", "v2/TurnSteerParams.json": "expectedTurnId", "v2/ThreadItemsListResponse.json": "clientId"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("schema contract missing %s in %s: %v", field, file, err)
		}
	}
}

// This probe starts an isolated native server and exercises its queue without
// requesting inference, touching the user's shared daemon, or answering approvals.
func TestNativeCodexProtocol(t *testing.T) {
	if os.Getenv("TWOMUX_PROTOCOL_SMOKE") != "1" {
		t.Skip("set TWOMUX_PROTOCOL_SMOKE=1 for native app-server probe")
	}
	dir, err := os.MkdirTemp("/tmp", "2mux-protocol-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "codex.sock")
	if err := os.Mkdir(filepath.Join(dir, "codex-home"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--listen", "unix://"+socket)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CODEX_HOME="+filepath.Join(dir, "codex-home"))
	log, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()
	end := time.Now().Add(10 * time.Second)
	for {
		if _, err = os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(end) {
			data, _ := os.ReadFile(filepath.Join(dir, "server.log"))
			t.Fatalf("socket missing: %s", data)
		}
		time.Sleep(50 * time.Millisecond)
	}
	info, _ := os.Stat(socket)
	t.Logf("isolated socket mode: %o", info.Mode().Perm())
	deadline, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	rpc, err := dialCodex(deadline, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.close()
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err = rpc.call(deadline, "thread/start", map[string]any{"cwd": dir, "sandbox": "read-only", "approvalPolicy": "on-request"}, &started); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"threadId": started.Thread.ID, "clientUserMessageId": "2mux-probe-1", "input": []any{map[string]string{"type": "text", "text": "Protocol probe; no turn is started."}}}
	var first, second json.RawMessage
	if err = rpc.call(deadline, "thread/queue/add", params, &first); err != nil {
		t.Fatal(err)
	}
	if err = rpc.call(deadline, "thread/queue/add", params, &second); err != nil {
		t.Logf("duplicate ID rejected: %v", err)
	}
	var listed struct {
		Data []struct {
			ID       string `json:"id"`
			ClientID string `json:"clientUserMessageId"`
		} `json:"data"`
	}
	if err = rpc.call(deadline, "thread/queue/list", map[string]string{"threadId": started.Thread.ID}, &listed); err != nil {
		t.Fatal(err)
	}
	t.Logf("duplicate-ID queue length: %d; dedup is not assumed", len(listed.Data))
	if len(listed.Data) == 0 {
		t.Fatal("queue entry missing")
	}
	for _, e := range listed.Data {
		if e.ClientID != "2mux-probe-1" {
			t.Fatal("client ID not retained")
		}
	}
	// A second connection must resume the same thread and observe its queue.
	// This checks connection recovery, not approval routing or queue execution.
	other, err := dialCodex(deadline, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	var resumed struct {
		Thread struct {
			ID  string `json:"id"`
			CWD string `json:"cwd"`
		} `json:"thread"`
	}
	if err := other.call(deadline, "thread/resume", map[string]string{"threadId": started.Thread.ID}, &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Thread.ID != started.Thread.ID || resumed.Thread.CWD != dir {
		t.Fatalf("resume identity: %+v", resumed)
	}
	rpc.close()
	other.close()
	cancel()
	_ = cmd.Wait()
	_ = os.Remove(socket)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	cmd = exec.CommandContext(ctx, "codex", "app-server", "--listen", "unix://"+socket)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, append(os.Environ(), "CODEX_HOME="+filepath.Join(dir, "codex-home")), log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var recovered *codexRPC
	end = time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		connectCtx, stop := rpcTimeout()
		recovered, err = dialCodex(connectCtx, socket)
		stop()
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.close()
	if err := recovered.call(deadline, "thread/resume", map[string]string{"threadId": started.Thread.ID}, &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Thread.ID != started.Thread.ID {
		t.Fatal("thread identity changed after restart")
	}
	t.Log("reconnect and thread/resume after isolated app-server restart passed")
	t.Log("WebSocket initialize, isolated thread and queue/list passed; no inference requested")
}
