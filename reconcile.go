package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"time"
)

const channelAckTimeout = 90 * time.Second

func expireChannelReceipts(dir, role string, now time.Time) error {
	lock, ok, err := tryLock(filepath.Join(dir, "queue.lock"))
	if err != nil || !ok {
		return err
	}
	defer lock.Close()
	messages, err := readMessages(dir)
	if err != nil {
		return err
	}
	for _, m := range messages {
		if m.To != role || m.Transport != "claude-channel" || m.Status != statusSubmitted {
			continue
		}
		submitted := m.SubmittedAt
		if submitted.IsZero() {
			submitted = m.Created
		}
		if now.Sub(submitted) < channelAckTimeout {
			continue
		}
		m.Status, m.Error = statusUncertain, "Claude Channel receipt timeout; registration may be blocked or the reviewer has not acknowledged it; inspect the native TUI before resolve"
		if err := writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m); err != nil {
			return err
		}
	}
	return nil
}

// Reconciliation only observes the existing transport. Finding a queue entry
// proves submission; finding the exact client ID in a user item proves receipt.
// An absent or incomplete result never triggers another submission.
func (t *nativeTransport) reconcile() error {
	messages, err := readMessages(t.dir)
	if err != nil {
		return err
	}
	pending := map[string]bool{}
	for _, m := range messages {
		if m.To == roleWorker && m.Transport == "codex" && (m.Status == statusSubmitted || m.Status == statusSending || m.Status == statusUncertain) {
			pending[m.ID] = true
		}
	}
	if len(pending) == 0 {
		return nil
	}
	queued, accepted := map[string]bool{}, map[string]bool{}
	for _, method := range []string{"thread/queue/list", "thread/items/list"} {
		cursor := ""
		for page := 0; ; page++ {
			if page == 100 {
				return errors.New("codex history limit reached; receipts remain unresolved")
			}
			params := map[string]any{"threadId": t.cfg.CodexThread, "limit": 100}
			if cursor != "" {
				params["cursor"] = cursor
			}
			var result struct {
				Data       []json.RawMessage `json:"data"`
				NextCursor string            `json:"nextCursor"`
			}
			ctx, cancel := rpcTimeout()
			err := t.rpc.call(ctx, method, params, &result)
			cancel()
			if err != nil {
				return err
			}
			for _, data := range result.Data {
				if method == "thread/queue/list" {
					var entry struct {
						ID string `json:"clientUserMessageId"`
					}
					if err := json.Unmarshal(data, &entry); err != nil {
						return err
					}
					queued[entry.ID] = true
				} else {
					var entry struct {
						Item struct {
							Type string `json:"type"`
							ID   string `json:"clientId"`
						} `json:"item"`
					}
					if err := json.Unmarshal(data, &entry); err != nil {
						return err
					}
					if entry.Item.Type == "userMessage" {
						accepted[entry.Item.ID] = true
					}
				}
			}
			if result.NextCursor == "" {
				break
			}
			if result.NextCursor == cursor {
				return errors.New("codex repeated a history cursor")
			}
			cursor = result.NextCursor
		}
	}
	lock, err := waitLock(filepath.Join(t.dir, "queue.lock"), 5*time.Second, "queue busy")
	if err != nil {
		return err
	}
	defer lock.Close()
	// Read again under the lock: an explicit ack or resolve can race the reads.
	messages, err = readMessages(t.dir)
	if err != nil {
		return err
	}
	for _, m := range messages {
		if !pending[m.ID] || m.Status == statusAccepted || m.Status == statusDelivered || m.Status == statusQueued {
			continue
		}
		switch {
		case accepted[m.ID]:
			m.Status, m.Error = statusAccepted, ""
		case queued[m.ID]:
			m.Status, m.Error = statusSubmitted, ""
		default:
			m.Status, m.Error = statusUncertain, "not found in Codex queue/history after reconnect; inspect the agent before resolve"
		}
		if err := writeJSON(filepath.Join(t.dir, "messages", m.ID+".json"), m); err != nil {
			return err
		}
	}
	return nil
}

// Channels have no transport receipt. After a process restart, only the
// explicit ack tool can prove receipt; an unacknowledged send stays uncertain.
func reconcileChannel(dir, role string) error {
	lock, err := waitLock(filepath.Join(dir, "queue.lock"), 5*time.Second, "queue busy")
	if err != nil {
		return err
	}
	defer lock.Close()
	messages, err := readMessages(dir)
	if err != nil {
		return err
	}
	for _, m := range messages {
		if m.To == role && m.Transport == "claude-channel" && (m.Status == statusSubmitted || m.Status == statusSending) {
			m.Status, m.Error = statusUncertain, "channel restarted without an acknowledgement; inspect the reviewer before resolve"
			if err := writeJSON(filepath.Join(dir, "messages", m.ID+".json"), m); err != nil {
				return err
			}
		}
	}
	return nil
}
