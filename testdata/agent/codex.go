package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/coder/websocket"
)

type fixtureThread struct {
	ID        string `json:"id"`
	CWD       string `json:"cwd"`
	Name      string `json:"name"`
	Turns     []any  `json:"turns"`
	Status    any    `json:"status"`
	owner     *websocket.Conn
	persisted bool
}

func runCodexServer(dir, socket string) error {
	os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	var mu sync.Mutex
	threads := map[string]*fixtureThread{}
	save := func(thread *fixtureThread) error {
		data, err := json.Marshal(thread)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "thread-"+thread.ID+".json"), data, 0600); err != nil {
			return err
		}
		thread.persisted = true
		return nil
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		defer func() {
			mu.Lock()
			defer mu.Unlock()
			// A never-saved thread disappears when its creator disconnects.
			// Persisted metadata or a real turn is required across reconnects.
			for id, thread := range threads {
				if thread.owner == conn && !thread.persisted && len(thread.Turns) == 0 {
					delete(threads, id)
				}
			}
		}()
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					ThreadID, CWD, Name, ClientUserMessageID string
					Input                                    []struct{ Text string }
				} `json:"params"`
			}
			if json.Unmarshal(data, &request) != nil {
				return
			}
			if request.Method == "initialized" {
				continue
			}
			mu.Lock()
			result := any(map[string]any{})
			var failure any
			thread := threads[request.Params.ThreadID]
			if thread == nil && request.Params.ThreadID != "" {
				stored, err := os.ReadFile(filepath.Join(dir, "thread-"+request.Params.ThreadID+".json"))
				var saved fixtureThread
				if err == nil && json.Unmarshal(stored, &saved) == nil {
					saved.persisted = true
					thread = &saved
					threads[saved.ID] = thread
				}
			}
			switch request.Method {
			case "initialize":
			case "thread/start":
				var id [16]byte
				if _, err := rand.Read(id[:]); err != nil {
					panic(err)
				}
				thread = &fixtureThread{ID: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), CWD: request.Params.CWD, Status: map[string]string{"type": "idle"}, Turns: []any{}, owner: conn}
				threads[thread.ID] = thread
				result = map[string]any{"thread": thread}
			case "thread/resume":
				if thread == nil {
					failure = map[string]any{"code": -32600, "message": "no rollout found for thread id " + request.Params.ThreadID}
				} else {
					result = map[string]any{"thread": thread}
				}
			case "thread/name/set":
				if thread == nil {
					failure = map[string]any{"code": -32600, "message": "thread not loaded"}
				} else {
					thread.Name = request.Params.Name
					if err := save(thread); err != nil {
						panic(err)
					}
				}
			case "thread/queue/list", "thread/items/list":
				result = map[string]any{"data": []any{}}
			case "thread/queue/add":
				if thread == nil {
					failure = map[string]any{"code": -32600, "message": "thread not loaded"}
				} else {
					thread.Turns = append(thread.Turns, request.Params.Input)
					if err := save(thread); err != nil {
						panic(err)
					}
				}
			default:
				failure = map[string]any{"code": -32601, "message": "unsupported fixture method " + request.Method}
			}
			response := map[string]any{"id": request.ID, "result": result}
			if failure != nil {
				delete(response, "result")
				response["error"] = failure
			}
			data, _ = json.Marshal(response)
			mu.Unlock()
			if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
				return
			}
		}
	})}
	return server.Serve(listener)
}

func resumeCodexThread(socket, id string) (func(), error) {
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	conn, _, err := websocket.Dial(context.Background(), "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, err
	}
	close := func() { conn.CloseNow() }
	for i, method := range []string{"initialize", "thread/resume"} {
		params := map[string]any{"clientInfo": map[string]string{"name": "2mux_fixture_tui", "version": "1"}}
		if method == "thread/resume" {
			params = map[string]any{"threadId": id}
		}
		data, _ := json.Marshal(map[string]any{"id": i, "method": method, "params": params})
		if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
			close()
			return nil, err
		}
		_, data, err = conn.Read(context.Background())
		if err != nil {
			close()
			return nil, err
		}
		var result struct{ Error any }
		if json.Unmarshal(data, &result) != nil || result.Error != nil {
			close()
			return nil, fmt.Errorf("%s", data)
		}
	}
	return close, nil
}
