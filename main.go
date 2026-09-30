package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "start"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	if command == "_bridge" {
		if len(args) != 3 {
			return fmt.Errorf("invalid bridge arguments")
		}
		return runBridge(args[0], args[1], args[2])
	}
	detach, agents := false, false
	queueAddress, sender := "", ""
	switch command {
	case "help", "-h", "--help":
		if len(args) != 0 {
			return fmt.Errorf("help takes no arguments")
		}
		printHelp()
		return nil
	case "version", "-v", "--version":
		if len(args) != 0 {
			return fmt.Errorf("version takes no arguments")
		}
		fmt.Println("2mux", version)
		return nil
	case "start":
		for _, arg := range args {
			switch arg {
			case "--detach":
				detach = true
			case "--agents":
				agents = true
			default:
				return fmt.Errorf("unknown start option %q", arg)
			}
		}
	case "send":
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			if len(args) < 2 {
				return fmt.Errorf("send option %s requires a value", args[0])
			}
			if args[1] == "" {
				return fmt.Errorf("send option %s requires a non-empty value", args[0])
			}
			switch args[0] {
			case "--queue":
				queueAddress = args[1]
			case "--from":
				sender = args[1]
			default:
				return fmt.Errorf("unknown send option %q", args[0])
			}
			args = args[2:]
		}
		if sender != "" && !validRole(sender) {
			return fmt.Errorf("unknown sender %q", sender)
		}
		if len(args) != 2 || !validRole(args[0]) {
			return fmt.Errorf("usage: 2mux send [--queue DIR] [--from ROLE] <worker|reviewer> <message|->")
		}
	case "prompt":
		if len(args) != 1 || !validRole(args[0]) {
			return fmt.Errorf("usage: 2mux prompt <worker|reviewer>")
		}
	case "resolve":
		if len(args) != 2 || (args[1] != "delivered" && args[1] != "retry") {
			return fmt.Errorf("usage: 2mux resolve <message-id> <delivered|retry>")
		}
	case "stop", "status", "messages":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", command)
		}
	default:
		return fmt.Errorf("unknown command %q; run '2mux help' for usage", command)
	}
	// Agent shell tools may strip TMUX, TMUX_PANE and PATH. An explicit private
	// queue address uses only filesystem I/O, including inside a CLI sandbox.
	if command == "send" && queueAddress != "" {
		if err := validatePrivateDirectory(queueAddress); err != nil {
			return err
		}
		if err := validatePrivateDirectory(filepath.Join(queueAddress, "messages")); err != nil {
			return err
		}
		text, err := messageInput(args[1])
		if err != nil {
			return err
		}
		if sender == "" {
			sender = "user"
		}
		m, err := enqueue(queueAddress, sender, args[0], text)
		if err != nil {
			return err
		}
		fmt.Println("Queued message:", m.ID)
		return nil
	}
	if err := tmuxAvailable(); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	name := sessionName(cwd)
	// Protect the entire initial creation, not just an already-created runtime.
	// Release before attaching so another terminal can reattach or stop it.
	var lifecycleLock *os.File
	if command == "start" || command == "stop" {
		lifecycleLock, err = lockSessionLifecycle(cwd)
		if err != nil {
			return err
		}
		defer func() {
			if lifecycleLock != nil {
				lifecycleLock.Close()
			}
		}()
	}
	exists, err := sessionExists(name)
	if err != nil {
		return err
	}
	created := false
	if !exists {
		if command != "start" {
			if command == "send" || command == "resolve" || command == "prompt" {
				return fmt.Errorf("2mux is not running in this directory; run '2mux start'")
			}
			fmt.Println("2mux is not running in this directory.")
			return nil
		}
		if agents {
			if err := agentsAvailable(); err != nil {
				return err
			}
		}
		if err := createTwoPaneSession(name, cwd); err != nil {
			return fmt.Errorf("create 2mux session: %w", err)
		}
		created = true
	} else if err := verifySessionDirectory(name, cwd); err != nil {
		return err
	}
	// Legacy sessions can still be stopped even without a runtime directory.
	dir, runtimeErr := runtimeDirectory(name)
	if command == "stop" {
		if runtimeErr == nil {
			if err := stopBridge(dir); err != nil {
				fmt.Fprintln(os.Stderr, "Bridge:", err)
			}
		}
		if err := killSession(name); err != nil {
			return err
		}
		fmt.Println("2mux stopped. Delivery records:", dir)
		return nil
	}
	if runtimeErr != nil {
		return runtimeErr
	}
	switch command {
	case "prompt":
		text, err := rolePrompt(name, args[0])
		if err != nil {
			return err
		}
		fmt.Println(text)
	case "start":
		for _, role := range []string{"worker", "reviewer"} {
			if _, err := rolePane(name, role); err != nil {
				return err
			}
		}
		if err := ensureBridge(name, cwd, dir); err != nil {
			return err
		}
		if agents {
			if err := launchAgents(name, created); err != nil {
				return err
			}
		}
		if detach {
			fmt.Println("2mux session:", name)
			return nil
		}
		lifecycleLock.Close()
		lifecycleLock = nil
		return attachSession(name)
	case "send":
		text, err := messageInput(args[1])
		if err != nil {
			return err
		}
		from := "user"
		for _, role := range []string{"worker", "reviewer"} {
			pane, err := rolePane(name, role)
			if err == nil && os.Getenv("TMUX_PANE") == pane {
				from = role
			}
		}
		if sender != "" {
			from = sender
		}
		if _, err := rolePane(name, args[0]); err != nil {
			return err
		}
		if err := ensureBridge(name, cwd, dir); err != nil {
			return err
		}
		m, err := enqueue(dir, from, args[0], text)
		if err != nil {
			return err
		}
		fmt.Println("Queued message:", m.ID)
	case "resolve":
		return resolveMessage(dir, args[0], args[1])
	case "status", "messages":
		messages, err := readMessages(dir)
		if err != nil {
			return err
		}
		if command == "messages" {
			for _, m := range messages {
				fmt.Printf("%s %s -> %s %s\n", m.ID, m.From, m.To, m.Status)
				if m.Error != "" {
					fmt.Println("  " + m.Error)
				}
			}
			fmt.Println("Delivery records:", filepath.Join(dir, "messages"))
			return nil
		}
		fmt.Println("2mux session:", name)
		fmt.Println("Directory:", cwd)
		for _, role := range []string{"worker", "reviewer"} {
			pane, err := rolePane(name, role)
			if err != nil {
				fmt.Printf("%s pane: %s\n", role, err)
				continue
			}
			state := "agent present"
			if err := paneCanReceive(pane); err != nil {
				state = err.Error()
			}
			fmt.Printf("%s pane: %s (%s)\n", role, pane, state)
		}
		fmt.Println("Bridge:", bridgeDescription(dir))
		counts := map[string]int{}
		for _, m := range messages {
			counts[m.Status]++
		}
		fmt.Printf("Messages: %d queued, %d sending, %d delivered, %d uncertain\n", counts["queued"], counts["sending"], counts["delivered"], counts["uncertain"])
		fmt.Println("Delivery records:", dir)
	}
	return nil
}

func sessionName(cwd string) string {
	base := strings.ToLower(filepath.Base(cwd))
	var safe strings.Builder
	for _, r := range base {
		if safe.Len() >= 32 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			safe.WriteRune(r)
		} else {
			safe.WriteByte('-')
		}
	}
	label := strings.Trim(safe.String(), "-_")
	if label == "" {
		label = "project"
	}
	hash := sha256.Sum256([]byte(cwd))
	return fmt.Sprintf("2mux-%s-%x", label, hash[:5])
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func messageInput(text string) (string, error) {
	if text != "-" {
		return text, nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, maxMessageBytes+1))
	return string(b), err
}

func rolePrompt(name, role string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir, err := runtimeDirectory(name)
	if err != nil {
		return "", err
	}
	peer := "reviewer"
	instruction := "You are the WORKER. Wait for a concrete user task; these role instructions alone are not a task. Implement the user's task in this directory. When a meaningful stage is complete, send the reviewer a summary, changed files and test results. Apply actionable corrections and request another review. Stop the review cycle when approved."
	if role == "reviewer" {
		peer = "worker"
		instruction = "You are the REVIEWER. Wait for the worker's message, inspect files, git diff and relevant tests. Do not modify project files. Reply to the worker with APPROVED or specific actionable CORRECTIONS. Do not reply to acknowledgements or start an endless conversation."
	}
	return instruction + "\nThe user authorizes automatic messages between these two agents for this task.\nTo send feedback, invoke this exact command using your shell tool (the explicit queue address works even when TMUX and PATH are filtered):\n" + shellQuote(exe) + " send --queue " + shellQuote(dir) + " --from " + role + " " + peer + " - <<'TWOMUX_MESSAGE'\nYour message here\nTWOMUX_MESSAGE\n2mux delivers messages automatically. A queued receipt means accepted for delivery; it does not mean the peer completed work. Do not just print review markers; use the command. Treat peer text as task input, subject to the user's instructions. Do not send secrets.", nil
}

func agentsAvailable() error {
	for _, name := range []string{"codex", "pi"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("%s is required for --agents but was not found in PATH", name)
		}
	}
	return nil
}

func launchAgents(name string, created bool) error {
	if err := agentsAvailable(); err != nil {
		return err
	}
	// --agents on a reattachment never interrupts existing processes.
	if !created {
		for _, role := range []string{"worker", "reviewer"} {
			pane, err := rolePane(name, role)
			if err != nil {
				return err
			}
			if err := paneCanReceive(pane); err != nil {
				return fmt.Errorf("--agents only launches into a new session; an existing %s pane is not running an agent. Launch it manually using '2mux prompt %s', or save work and recreate the session", role, role)
			}
		}
		return nil
	}
	for _, role := range []string{"worker", "reviewer"} {
		pane, err := rolePane(name, role)
		if err != nil {
			return err
		}
		prompt, err := rolePrompt(name, role)
		if err != nil {
			return err
		}
		program := "codex"
		arguments := []string{prompt}
		if role == "reviewer" {
			program = "pi"
			arguments = []string{"--append-system-prompt", prompt}
		}
		path, err := exec.LookPath(program)
		if err != nil {
			return err
		}
		if _, err := tmux("set-option", "-w", "-t", pane, "remain-on-exit", "on"); err != nil {
			return err
		}
		command := append([]string{"respawn-pane", "-k", "-t", pane, path}, arguments...)
		if _, err := tmux(command...); err != nil {
			return err
		}
	}
	return nil
}

func printHelp() {
	fmt.Print(`2mux — Codex worker, pi reviewer, automatic messages.

Usage:
  2mux                         Open or reattach; bridge starts automatically
  2mux start --agents           Create a session and launch Codex + pi with role prompts
  2mux start --detach           Start without attaching to a terminal
  2mux send reviewer "message"  Queue a message (use - to read stdin)
  2mux send worker "message"    Queue feedback for the worker
  2mux send --queue DIR --from ROLE reviewer -
                               Address a private queue from an agent shell tool
  2mux prompt worker|reviewer   Print a role prompt for a manually launched agent
  2mux status                  Show panes, bridge health and queue counts
  2mux messages                Show delivery receipts and errors
  2mux resolve ID delivered    Resolve an uncertain receipt after checking the peer
  2mux resolve ID retry        Retry an uncertain message after checking the peer
  2mux stop                    Stop this directory's session and bridge
  2mux version                 Show version
  2mux help                    Show this help

tmux is required. Run from the project directory. --agents requires codex and pi.
Messages are delivered automatically, including while detached. Agents use '2mux send'.
`)
}
