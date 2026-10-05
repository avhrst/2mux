package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Codex's Unix listener speaks WebSocket, not newline JSON. One reader
// multiplexes replies and events. Server requests are observed, never answered:
// decisions stay in the native TUI. Multi-client routing is an explicit live
// validation gate, not a property inferred from the schema or reader behavior.
type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("Codex RPC %d: %s", e.Code, e.Message) }

type codexRPC struct {
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	next    atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan rpcEnvelope
	events  chan rpcEnvelope
	done    chan struct{}
}

func dialCodex(ctx context.Context, socket string) (*codexRPC, error) {
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	c, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(16 << 20)
	readCtx, cancel := context.WithCancel(context.Background())
	r := &codexRPC{conn: c, ctx: readCtx, cancel: cancel, pending: map[uint64]chan rpcEnvelope{}, events: make(chan rpcEnvelope, 256), done: make(chan struct{})}
	go r.read()
	if err := r.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "2mux_bridge", "version": version}, "capabilities": map[string]any{"experimentalApi": true}}, nil); err != nil {
		r.close()
		return nil, err
	}
	if err := r.notify(ctx, "initialized", map[string]any{}); err != nil {
		r.close()
		return nil, err
	}
	return r, nil
}
func (r *codexRPC) close() { r.cancel(); r.conn.CloseNow(); <-r.done }
func (r *codexRPC) read() {
	defer close(r.done)
	for {
		_, data, err := r.conn.Read(r.ctx)
		if err != nil {
			return
		}
		var e rpcEnvelope
		if json.Unmarshal(data, &e) != nil {
			return
		}
		if e.Method == "" && len(e.ID) > 0 {
			var id uint64
			if json.Unmarshal(e.ID, &id) != nil {
				continue
			}
			r.mu.Lock()
			ch := r.pending[id]
			r.mu.Unlock()
			if ch != nil {
				select {
				case ch <- e:
				default:
				}
			}
		} else {
			// Text deltas and unrelated notifications are not state evidence.
			// Never let a fast model's stream delay request responses indefinitely.
			switch e.Method {
			case "turn/started", "turn/completed", "thread/status/changed", "thread/closed",
				"item/started", "item/completed", "item/commandExecution/requestApproval",
				"item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
			default:
				continue
			}
			select {
			case r.events <- e:
			case <-r.ctx.Done():
				return
			default:
				// A lost state event makes this connection unusable. Reconnect and
				// reconcile receipts instead of dropping evidence or blocking reads.
				r.conn.CloseNow()
				return
			}
		}
	}
}
func (r *codexRPC) notify(ctx context.Context, method string, params any) error {
	b, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return err
	}
	return r.conn.Write(ctx, websocket.MessageText, b)
}
func (r *codexRPC) call(ctx context.Context, method string, params, out any) error {
	id := r.next.Add(1)
	ch := make(chan rpcEnvelope, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }()
	b, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err = r.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return err
	}
	select {
	case e := <-ch:
		if e.Error != nil {
			return e.Error
		}
		if out != nil {
			return json.Unmarshal(e.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return fmt.Errorf("codex connection closed during %s", method)
	}
}
func rpcTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
