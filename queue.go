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
	statusSubmitted messageStatus = "submitted"
	statusAccepted  messageStatus = "accepted"
	statusRejected  messageStatus = "rejected"
)

type message struct {
	ID          string        `json:"id"`
	From        string        `json:"from"`
	To          string        `json:"to"`
	Text        string        `json:"text"`
	Created     time.Time     `json:"created"`
	Status      messageStatus `json:"status"`
	Error       string        `json:"error,omitempty"`
	Kind        string        `json:"kind,omitempty"`
	ReplyTo     string        `json:"reply_to,omitempty"`
	Verdict     string        `json:"verdict,omitempty"`
	Scope       string        `json:"scope,omitempty"`
	Transport   string        `json:"transport,omitempty"`
	Attempt     int           `json:"attempt,omitempty"`
	Steer       bool          `json:"steer,omitempty"`
	SubmittedAt time.Time     `json:"submitted_at,omitempty"`
	TurnRef     string        `json:"turn_ref,omitempty"`
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
		line = strings.TrimLeft(line, " \t")
		if strings.HasPrefix(line, headerPrefix) {
			return errors.New("message lines must not start with " + headerPrefix)
		}
		if strings.HasPrefix(line, communicationPrefix) {
			return errors.New("message lines must not start with " + communicationPrefix)
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

func enqueue(dir, from, to, text string, options ...sendOptions) (message, error) {
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
	if len(options) > 0 {
		m.Kind, m.ReplyTo, m.Verdict, m.Steer = options[0].Kind, options[0].ReplyTo, options[0].Verdict, options[0].Steer
	}
	lock, err := waitLock(filepath.Join(dir, "queue.lock"), 5*time.Second, "message queue is busy")
	if err != nil {
		return message{}, err
	}
	defer lock.Close()
	if err := validateConversation(dir, &m); err != nil {
		return message{}, err
	}
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
	case statusQueued, statusSending, statusDelivered, statusUncertain, statusSubmitted, statusAccepted, statusRejected:
	default:
		return m, fmt.Errorf("invalid message status in %s", name)
	}
	if err := validateMessageOptions(m); err != nil {
		return m, fmt.Errorf("invalid message metadata %s: %w", name, err)
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
	return deliverQueueTransport(dir, ready, deliver, nil)
}

func deliverQueueTransport(dir string, ready, deliver func(message) error, choose func(message) (string, error), owners ...func(message) bool) error {
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
		if len(owners) > 0 && !owners[0](m) {
			continue
		}
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
		if choose != nil && m.Transport == "" {
			if m.Attempt > 0 {
				m.Transport = "tmux"
			} else {
				transport, err := choose(m)
				if err != nil {
					blocked[m.To], lastError = true, err
					continue
				}
				m.Transport = transport
			}
			if err := writeJSON(path, m); err != nil {
				return err
			}
		}
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
		if m.Kind == "verdict" {
			if err := validateVerdictScope(dir, m); err != nil {
				lastError = err
				m.Status, m.Error = statusRejected, err.Error()
				if err := writeJSON(path, m); err != nil {
					return err
				}
				continue
			}
		}
		m.Status, m.Error = statusSending, ""
		m.Attempt++
		if err := writeJSON(path, m); err != nil {
			return err
		}
		err := deliver(m)
		m.Status = statusDelivered
		if m.Transport != "" && m.Transport != "tmux" {
			m.Status = statusSubmitted
			m.SubmittedAt = time.Now().UTC()
		}
		if err != nil {
			m.Status, m.Error = statusQueued, err.Error()
			var uncertain uncertainDelivery
			if errors.As(err, &uncertain) {
				m.Status = statusUncertain
			}
			var rejected rejectedDelivery
			if errors.As(err, &rejected) {
				m.Status = statusRejected
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
	return communicationHeading(m, "") + fmt.Sprintf("%s message %s from %s to %s]\n%s\n%s end of message %s]", headerPrefix, m.ID, m.From, m.To, m.Text, headerPrefix, m.ID)
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
		if (m.Status != statusDelivered && m.Status != statusAccepted) || m.Kind != "" || m.ReplyTo != "" || time.Since(m.Created) < olderThan {
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
