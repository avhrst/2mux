package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type sendOptions struct {
	Kind, ReplyTo, Verdict string
	Steer                  bool
}

func validMessageID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func validateMessageOptions(m message) error {
	if m.Kind != "" && m.Kind != "note" && m.Kind != "review_request" && m.Kind != "verdict" {
		return errors.New("kind must be note, review_request or verdict")
	}
	if m.ReplyTo != "" && !validMessageID(m.ReplyTo) {
		return errors.New("reply_to must be a message ID")
	}
	if m.Kind == "review_request" && (!validRole(m.From) || m.To != peerRole(m.From)) {
		return errors.New("review_request requires an agent sender and its peer recipient")
	}
	if m.Steer && m.To != roleWorker {
		return errors.New("--steer is only supported for the Codex worker")
	}
	if m.Kind == "verdict" {
		if m.ReplyTo == "" || !validRole(m.From) || (m.Verdict != "APPROVED" && m.Verdict != "CORRECTIONS") {
			return errors.New("verdict requires an agent sender, --reply-to and --verdict APPROVED|CORRECTIONS")
		}
	} else if m.Verdict != "" {
		return errors.New("--verdict requires --kind verdict")
	}
	if m.Attempt < 0 {
		return errors.New("attempt must not be negative")
	}
	if m.Transport != "" && m.Transport != "tmux" && m.Transport != "codex" && m.Transport != "claude-channel" {
		return errors.New("invalid transport")
	}
	if (m.Transport == "codex" && m.To != roleWorker) || (m.Transport == "claude-channel" && m.To != roleReviewer) {
		return errors.New("transport does not match recipient")
	}
	return nil
}
func validateConversation(dir string, m *message) error {
	if err := validateMessageOptions(*m); err != nil {
		return err
	}
	if m.Steer {
		cfg, err := readSessionConfig(dir)
		if err != nil {
			return err
		}
		if !cfg.CodexAPI {
			return errors.New("--steer requires a --codex-api session")
		}
		state := readAgentState(dir, roleWorker)
		if state.TurnID == "" {
			return errors.New("--steer requires an active Codex turn")
		}
		m.TurnRef = state.TurnID
	}
	if m.Kind == "review_request" {
		scope, err := currentScope(dir)
		if err != nil {
			return err
		}
		m.Scope = scope
	}
	if m.ReplyTo == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "messages", m.ReplyTo+".json"))
	if err != nil {
		return fmt.Errorf("reply target missing: %w", err)
	}
	parent, err := parseMessage(m.ReplyTo+".json", data)
	if err != nil {
		return err
	}
	if parent.From != m.To || parent.To != m.From {
		return errors.New("reply sender/recipient do not match the request")
	}
	m.Scope = parent.Scope
	if m.Kind == "verdict" {
		if parent.Kind != "review_request" {
			return errors.New("verdict target is not a review request")
		}
		if parent.Scope != "" {
			now, err := currentScope(dir)
			if err != nil {
				return err
			}
			if now != parent.Scope {
				return errors.New("review scope changed; request a new review before sending a verdict")
			}
		}
		messages, err := readMessages(dir)
		if err != nil {
			return err
		}
		for _, other := range messages {
			if other.Kind == "verdict" && other.ReplyTo == m.ReplyTo {
				return errors.New("request already has a verdict")
			}
		}
	}
	return nil
}

func validateVerdictScope(dir string, m message) error {
	data, err := os.ReadFile(filepath.Join(dir, "messages", m.ReplyTo+".json"))
	if err != nil {
		return err
	}
	parent, err := parseMessage(m.ReplyTo+".json", data)
	if err != nil {
		return err
	}
	if parent.Kind != "review_request" || parent.From != m.To || parent.To != m.From || parent.Scope != m.Scope {
		return errors.New("verdict request identity mismatch")
	}
	if m.Scope != "" {
		scope, err := currentScope(dir)
		if err != nil {
			return err
		}
		if scope != m.Scope {
			return errors.New("stale review verdict; scope changed")
		}
	}
	return nil
}

// Scope includes HEAD, staged/unstaged changes and all non-ignored untracked
// paths. Symlinks are hashed as links rather than following them outside cwd.
func currentScope(dir string) (string, error) {
	cfg, err := readSessionConfig(dir)
	if err != nil {
		return "", err
	}
	if cfg.CWD == "" {
		return "", nil
	}
	git := func(args ...string) ([]byte, error) {
		c := exec.Command("git", args...)
		c.Dir = cfg.CWD
		return c.Output()
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", nil
	}
	diff, err := git("diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD")
	if err != nil {
		return "", err
	}
	paths, err := git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	part := func(data []byte) {
		_ = binary.Write(h, binary.BigEndian, uint64(len(data)))
		h.Write(data)
	}
	part(head)
	part(diff)
	for _, p := range strings.Split(string(paths), "\x00") {
		if p == "" {
			continue
		}
		part([]byte(p))
		path := filepath.Join(cfg.CWD, p)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		part([]byte(info.Mode().String()))
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			part([]byte(target))
		} else if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return "", err
			}
			_ = binary.Write(h, binary.BigEndian, uint64(info.Size()))
			n, err := io.Copy(h, f)
			f.Close()
			if err != nil {
				return "", err
			}
			if n != info.Size() {
				return "", errors.New("untracked file changed during scope calculation")
			}
		}
	}
	return strings.TrimSpace(string(head)) + ":" + hex.EncodeToString(h.Sum(nil)), nil
}
