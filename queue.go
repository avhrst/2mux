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

const maxMessageBytes = 64 * 1024

type message struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Text    string    `json:"text"`
	Created time.Time `json:"created"`
	Status  string    `json:"status"`
	Error   string    `json:"error,omitempty"`
}

func validRole(role string) bool { return role == "worker" || role == "reviewer" }

func validSender(sender string) bool { return sender == "user" || validRole(sender) }

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
	m := message{ID: id, From: from, To: to, Text: text, Created: time.Now().UTC(), Status: "queued"}
	err = writeJSON(filepath.Join(dir, "messages", id+".json"), m)
	return m, err
}

func readMessages(dir string) ([]message, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "messages"))
	if err != nil {
		return nil, err
	}
	var messages []message
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "messages", entry.Name()))
		if err != nil {
			return nil, err
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("invalid message %s: %w", entry.Name(), err)
		}
		if len(m.ID) != 32 || entry.Name() != m.ID+".json" || !validRole(m.To) {
			return nil, fmt.Errorf("invalid message record %s", entry.Name())
		}
		if _, err := hex.DecodeString(m.ID); err != nil {
			return nil, fmt.Errorf("invalid message ID in %s", entry.Name())
		}
		// Sender text is included in the terminal header, and timestamps define
		// delivery order. Both must be valid even when the body is well-formed.
		if !validSender(m.From) || m.Created.IsZero() {
			return nil, fmt.Errorf("invalid message metadata in %s", entry.Name())
		}
		if err := validateText(m.Text); err != nil {
			return nil, fmt.Errorf("invalid message %s: %w", entry.Name(), err)
		}
		switch m.Status {
		case "queued", "sending", "delivered", "uncertain":
		default:
			return nil, fmt.Errorf("invalid message status in %s", entry.Name())
		}
		messages = append(messages, m)
	}
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].Created.Equal(messages[j].Created) {
			return messages[i].ID < messages[j].ID
		}
		return messages[i].Created.Before(messages[j].Created)
	})
	return messages, nil
}

func deliverQueue(dir string, deliver func(message) error) error {
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
		if m.Status == "sending" {
			// A previous process could have submitted before it crashed.
			m.Status, m.Error = "uncertain", "bridge stopped during delivery; inspect the receiving agent before resending"
			if err := writeJSON(path, m); err != nil {
				return err
			}
		}
		if m.Status == "uncertain" {
			blocked[m.To] = true
			lastError = fmt.Errorf("delivery %s is uncertain; inspect '2mux messages'", m.ID)
			continue
		}
		if m.Status != "queued" || blocked[m.To] {
			continue
		}
		// Bound a batch so health updates and shutdown remain responsive.
		if attempts == 8 {
			break
		}
		attempts++
		m.Status, m.Error = "sending", ""
		if err := writeJSON(path, m); err != nil {
			return err
		}
		err := deliver(m)
		m.Status = "delivered"
		if err != nil {
			m.Status, m.Error = "queued", err.Error()
			var uncertain uncertainDelivery
			if errors.As(err, &uncertain) {
				m.Status = "uncertain"
			}
			blocked[m.To], lastError = true, err
		}
		if err := writeJSON(path, m); err != nil {
			return err
		}
	}
	return lastError
}

func formatMessage(m message) string {
	return fmt.Sprintf("[2mux message %s from %s to %s]\n%s", m.ID, m.From, m.To, m.Text)
}

func resolveMessage(dir, id, action string) error {
	var lock *os.File
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f, ok, err := tryLock(filepath.Join(dir, "queue.lock"))
		if err != nil {
			return err
		}
		if ok {
			lock = f
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lock == nil {
		return errors.New("message queue is busy; try resolving again")
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
		if m.Status != "uncertain" && m.Status != "sending" {
			return errors.New("only uncertain or interrupted messages can be resolved")
		}
		m.Status, m.Error = "delivered", ""
		if action == "retry" {
			m.Status = "queued"
		}
		return writeJSON(filepath.Join(dir, "messages", id+".json"), m)
	}
	return fmt.Errorf("unknown message %q", id)
}
