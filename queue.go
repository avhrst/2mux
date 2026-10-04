package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxMessageBytes = 64 * 1024
	headerPrefix    = "[2mux"
	// Bound a delivery batch so health updates and shutdown stay responsive.
	deliveryBatchLimit = 8
)

const (
	roleWorker   = "worker"
	roleReviewer = "reviewer"
	senderUser   = "user"
)

// roles lists both agent roles in their fixed pane order.
var roles = []string{roleWorker, roleReviewer}

func peerRole(role string) string {
	if role == roleWorker {
		return roleReviewer
	}
	return roleWorker
}

type messageStatus string

const (
	statusQueued    messageStatus = "queued"
	statusSending   messageStatus = "sending"
	statusDelivered messageStatus = "delivered"
	statusUncertain messageStatus = "uncertain"
)

type message struct {
	ID      string        `json:"id"`
	From    string        `json:"from"`
	To      string        `json:"to"`
	Text    string        `json:"text"`
	Created time.Time     `json:"created"`
	Status  messageStatus `json:"status"`
	Error   string        `json:"error,omitempty"`
}

func validRole(role string) bool { return role == roleWorker || role == roleReviewer }

func validSender(sender string) bool { return sender == senderUser || validRole(sender) }

func validateText(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("message must not be empty")
	}
	if len(text) > maxMessageBytes || !utf8.ValidString(text) {
		return errors.New("message must be valid UTF-8 and at most 64 KiB")
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return errors.New("message contains terminal control characters")
		}
		// Bidi overrides can make the pane display text other than what is sent.
		if unicode.Is(unicode.Bidi_Control, r) {
			return errors.New("message contains bidirectional control characters")
		}
	}
	// A body line that looks like a 2mux header could impersonate another
	// sender, such as the user, inside a single delivered message.
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), headerPrefix) {
			return errors.New("message lines must not start with " + headerPrefix)
		}
	}
	return nil
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// A same-directory rename makes complete records visible atomically.
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func enqueue(dir, from, to, text string) (message, error) {
	if !validSender(from) {
		return message{}, fmt.Errorf("unknown sender %q; use user, worker or reviewer", from)
	}
	if !validRole(to) {
		return message{}, fmt.Errorf("unknown recipient %q; use worker or reviewer", to)
	}
	if err := validateText(text); err != nil {
		return message{}, err
	}
	id, err := randomID()
	if err != nil {
		return message{}, err
	}
	m := message{ID: id, From: from, To: to, Text: text, Created: time.Now().UTC(), Status: statusQueued}
	err = writeJSON(filepath.Join(dir, "messages", id+".json"), m)
	return m, err
}

// readMessages fails on any corrupt record, so delivery stays fail-closed.
func readMessages(dir string) ([]message, error) {
	messages, problems, err := scanMessages(dir)
	if err == nil && len(problems) > 0 {
		err = errors.New(problems[0])
	}
	return messages, err
}

// scanMessages returns valid records and describes corrupt ones separately,
// so diagnostic commands can still report the rest of the queue.
func scanMessages(dir string) ([]message, []string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "messages"))
	if err != nil {
		return nil, nil, err
	}
	var messages []message
	var problems []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "messages", entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		m, err := parseMessage(entry.Name(), data)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		messages = append(messages, m)
	}
	sortMessages(messages)
	return messages, problems, nil
}

func parseMessage(name string, data []byte) (message, error) {
	var m message
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("invalid message %s: %w", name, err)
	}
	if len(m.ID) != 32 || name != m.ID+".json" || !validRole(m.To) {
		return m, fmt.Errorf("invalid message record %s", name)
	}
	if _, err := hex.DecodeString(m.ID); err != nil {
		return m, fmt.Errorf("invalid message ID in %s", name)
	}
	// Sender text is included in the terminal header, and timestamps define
	// delivery order. Both must be valid even when the body is well-formed.
	if !validSender(m.From) || m.Created.IsZero() {
		return m, fmt.Errorf("invalid message metadata in %s", name)
	}
	if err := validateText(m.Text); err != nil {
		return m, fmt.Errorf("invalid message %s: %w", name, err)
	}
	switch m.Status {
	case statusQueued, statusSending, statusDelivered, statusUncertain:
	default:
		return m, fmt.Errorf("invalid message status in %s", name)
	}
	return m, nil
}

func sortMessages(messages []message) {
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].Created.Equal(messages[j].Created) {
			return messages[i].ID < messages[j].ID
		}
		return messages[i].Created.Before(messages[j].Created)
	})
}

// deliverQueue attempts queued messages in order. ready, when non-nil, is
// checked before a record is marked sending, so a recipient that cannot
// receive does not cause repeated record writes on every bridge tick.
func deliverQueue(dir string, ready, deliver func(message) error) error {
	lock, ok, err := tryLock(filepath.Join(dir, "queue.lock"))
	if err != nil || !ok {
		return err
	}
	defer lock.Close()
	messages, err := readMessages(dir)
	if err != nil {
		return err
	}
	var lastError error
	blocked := map[string]bool{}
	attempts := 0
	for _, m := range messages {
		path := filepath.Join(dir, "messages", m.ID+".json")
		if m.Status == statusSending {
			// A previous process could have submitted before it crashed.
			m.Status, m.Error = statusUncertain, "bridge stopped during delivery; inspect the receiving agent before resending"
			if err := writeJSON(path, m); err != nil {
				return err
			}
		}
		if m.Status == statusUncertain {
			blocked[m.To] = true
			lastError = fmt.Errorf("delivery %s is uncertain; inspect '2mux messages'", m.ID)
			continue
		}
		if m.Status != statusQueued || blocked[m.To] {
			continue
		}
		if attempts == deliveryBatchLimit {
			break
		}
		attempts++
		if ready != nil {
			if err := ready(m); err != nil {
				blocked[m.To], lastError = true, err
				if m.Error != err.Error() {
					m.Error = err.Error()
					if err := writeJSON(path, m); err != nil {
						return err
					}
				}
				continue
			}
		}
		m.Status, m.Error = statusSending, ""
		if err := writeJSON(path, m); err != nil {
			return err
		}
		err := deliver(m)
		m.Status = statusDelivered
		if err != nil {
			m.Status, m.Error = statusQueued, err.Error()
			var uncertain uncertainDelivery
			if errors.As(err, &uncertain) {
				m.Status = statusUncertain
			}
			blocked[m.To], lastError = true, err
		}
		if err := writeJSON(path, m); err != nil {
			return err
		}
	}
	return lastError
}

// The closing marker contains the random ID, which a sender cannot know when
// writing the body, so the receiving agent can see where peer text ends.
func formatMessage(m message) string {
	return fmt.Sprintf("%s message %s from %s to %s]\n%s\n%s end of message %s]", headerPrefix, m.ID, m.From, m.To, m.Text, headerPrefix, m.ID)
}

// archiveDelivered moves old delivered records out of the scanned directory,
// so the bridge does not reread a growing history on every tick.
func archiveDelivered(dir string, olderThan time.Duration) error {
	lock, ok, err := tryLock(filepath.Join(dir, "queue.lock"))
	if err != nil || !ok {
		return err
	}
	defer lock.Close()
	messages, err := readMessages(dir)
	if err != nil {
		return err
	}
	archive := filepath.Join(dir, "messages", "archive")
	for _, m := range messages {
		if m.Status != statusDelivered || time.Since(m.Created) < olderThan {
			continue
		}
		if err := os.Mkdir(archive, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		name := m.ID + ".json"
		if err := os.Rename(filepath.Join(dir, "messages", name), filepath.Join(archive, name)); err != nil {
			return err
		}
	}
	return nil
}

func resolveMessage(dir, id, action string) error {
	lock, err := waitLock(filepath.Join(dir, "queue.lock"), 3*time.Second, "message queue is busy; try resolving again")
	if err != nil {
		return err
	}
	defer lock.Close()
	messages, err := readMessages(dir)
	if err != nil {
		return err
	}
	for _, m := range messages {
		if m.ID != id {
			continue
		}
		if m.Status != statusUncertain && m.Status != statusSending {
			return errors.New("only uncertain or interrupted messages can be resolved")
		}
		m.Status, m.Error = statusDelivered, ""
		if action == "retry" {
			m.Status = statusQueued
		}
		return writeJSON(filepath.Join(dir, "messages", id+".json"), m)
	}
	return fmt.Errorf("unknown message %q", id)
}
