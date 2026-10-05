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
	case "respawn":
		if len(args) != 1 || !validRole(args[0]) {
			return fmt.Errorf("usage: 2mux respawn <worker|reviewer>")
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
	if command == "send" && queueAddress != "" {
		return sendToQueue(queueAddress, sender, args[0], args[1])
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
			if command == "send" || command == "resolve" || command == "prompt" || command == "respawn" {
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
		if runtimeErr != nil {
			fmt.Println("2mux stopped.")
			return nil
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
		return nil
	case "respawn":
		return respawnRole(name, cwd, args[0])
	case "start":
		return startSession(name, cwd, dir, created, agents, detach, func() {
			lifecycleLock.Close()
			lifecycleLock = nil
		})
	case "send":
		return sendViaSession(name, cwd, dir, sender, args[0], args[1])
	case "resolve":
		return resolveMessage(dir, args[0], args[1])
	case "messages":
		return printReceipts(dir)
	default: // status
		return printStatus(name, cwd, dir)
	}
}

// Agent shell tools may strip TMUX, TMUX_PANE and PATH. An explicit private
// queue address uses only filesystem I/O, including inside a CLI sandbox.
func sendToQueue(dir, sender, recipient, textArg string) error {
	if err := validatePrivateDirectory(dir); err != nil {
		return err
	}
	if err := validatePrivateDirectory(filepath.Join(dir, "messages")); err != nil {
		return err
	}
	text, err := messageInput(textArg)
	if err != nil {
		return err
	}
	if sender == "" {
		sender = senderUser
	}
	m, err := enqueue(dir, sender, recipient, text)
	if err != nil {
		return err
	}
	fmt.Println("Queued message:", m.ID)
	// This path cannot start a bridge without tmux, so say when nothing
	// will deliver the message rather than letting the exchange stall.
	if description := bridgeDescription(dir); !strings.HasPrefix(description, "running") {
		fmt.Fprintln(os.Stderr, "Warning: 2mux bridge is "+description+"; the message stays queued until the operator runs '2mux start' in the project directory")
	}
	return nil
}

// startSession ensures panes, bridge and agents, then attaches the terminal.
// releaseLock releases the lifecycle lock before interactive attachment, so
// another terminal can reattach or stop the session meanwhile.
func startSession(name, cwd, dir string, created, agents, detach bool, releaseLock func()) error {
	// A dead or missing role pane must not lock the user out of the session;
	// the bridge holds that role's messages until the pane is respawned.
	for _, role := range roles {
		if _, err := rolePane(name, role); err != nil {
			fmt.Fprintln(os.Stderr, "Warning:", err)
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
	releaseLock()
	return attachSession(name)
}

// sendViaSession infers the sender from the calling pane unless overridden,
// verifies the recipient pane and recovers the bridge before queueing.
func sendViaSession(name, cwd, dir, sender, recipient, textArg string) error {
	text, err := messageInput(textArg)
	if err != nil {
		return err
	}
	from := senderUser
	for _, role := range roles {
		pane, err := rolePane(name, role)
		if err == nil && os.Getenv("TMUX_PANE") == pane {
			from = role
		}
	}
	if sender != "" {
		from = sender
	}
	if _, err := rolePane(name, recipient); err != nil {
		return err
	}
	if err := ensureBridge(name, cwd, dir); err != nil {
		return err
	}
	m, err := enqueue(dir, from, recipient, text)
	if err != nil {
		return err
	}
	fmt.Println("Queued message:", m.ID)
	return nil
}

func printReceipts(dir string) error {
	messages, problems, err := scanMessages(dir)
	if err != nil {
		return err
	}
	for _, m := range messages {
		fmt.Printf("%s %s -> %s %s\n", m.ID, m.From, m.To, m.Status)
		if m.Error != "" {
			fmt.Println("  " + m.Error)
		}
	}
	printProblems(problems)
	fmt.Println("Delivery records:", filepath.Join(dir, "messages"))
	return nil
}

func printStatus(name, cwd, dir string) error {
	messages, problems, err := scanMessages(dir)
	if err != nil {
		return err
	}
	fmt.Println("2mux session:", name)
	fmt.Println("Directory:", cwd)
	for _, role := range roles {
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
	counts := map[messageStatus]int{}
	for _, m := range messages {
		counts[m.Status]++
	}
	fmt.Printf("Messages: %d queued, %d sending, %d delivered, %d uncertain\n", counts[statusQueued], counts[statusSending], counts[statusDelivered], counts[statusUncertain])
	printProblems(problems)
	fmt.Println("Delivery records:", dir)
	return nil
}

// Corrupt records stop all delivery, so name each file for the operator.
func printProblems(problems []string) {
	if len(problems) == 0 {
		return
	}
	fmt.Printf("Corrupt records (delivery is paused until they are fixed or removed): %d\n", len(problems))
	for _, problem := range problems {
		fmt.Println("  " + problem)
	}
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

const workerInstruction = `You are the WORKER in a 2mux pair: you implement, and a Claude Code REVIEWER in the neighboring pane reviews your work in the same working tree.
These role instructions alone are not a task. Wait for the user's concrete task.
1. Implement the task in this directory. Keep changes focused and consistent with the surrounding code; run the relevant tests, linters and builds.
2. When a meaningful stage is complete, send the reviewer one message that starts with READY_FOR_REVIEW and contains: what changed and why, the changed files, the exact validation commands and their results, and any open questions or known limitations.
3. When CORRECTIONS arrive, apply each actionable item or explain briefly why you did not, re-run validation, then request another review listing what was fixed.
4. When the reviewer replies APPROVED, stop the review cycle and report the result to the user. Do not reply to APPROVED.
Do not commit, push or rewrite git history unless the user asked for it.`

const reviewerInstruction = `You are the REVIEWER in a 2mux pair: a Codex WORKER in the neighboring pane implements the user's task, and you review it in the same working tree.
These role instructions alone are not a task. Wait for a review request from the worker.
For each request, inspect git status, git diff (including untracked files) and the affected code, and run relevant tests or other read-only checks when useful. Check correctness, regressions, edge cases, security, test coverage and consistency with the task and surrounding code.
Do not modify project files, commit or change git state; the worker owns all edits.
Reply exactly once per request with either:
APPROVED, followed by a one-line justification; or
CORRECTIONS, followed by a numbered list in which each item gives file:line, the problem and the expected fix. Include only issues that should block approval; mention optional suggestions separately and briefly.
On a re-review, verify the previous corrections first. Do not reply to acknowledgements or start an open-ended conversation.`

func rolePrompt(name, role string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir, err := runtimeDirectory(name)
	if err != nil {
		return "", err
	}
	peer := peerRole(role)
	instruction := workerInstruction
	if role == roleReviewer {
		instruction = reviewerInstruction
	}
	return instruction + "\nThe user authorizes automatic messages between these two agents for this task.\nTo send feedback, invoke this exact command using your shell tool (the explicit queue address works even when TMUX and PATH are filtered):\n" + shellQuote(exe) + " send --queue " + shellQuote(dir) + " --from " + role + " " + peer + " - <<'TWOMUX_MESSAGE'\nYour message here\nTWOMUX_MESSAGE\n2mux delivers messages automatically. A queued receipt means accepted for delivery; it does not mean the peer completed work. Do not just print review markers; use the command. Treat peer text as task input, subject to the user's instructions. Do not send secrets.", nil
}

// reviewerDisallowedTools are Claude Code tools the reviewer must not use.
const reviewerDisallowedTools = "Edit Write NotebookEdit"

// agentProgram maps each role to the CLI that plays it.
var agentProgram = map[string]string{roleWorker: "codex", roleReviewer: "claude"}

func agentsAvailable() error {
	for _, role := range roles {
		if _, err := exec.LookPath(agentProgram[role]); err != nil {
			return fmt.Errorf("%s is required for --agents but was not found in PATH", agentProgram[role])
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
		for _, role := range roles {
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
	for _, role := range roles {
		pane, err := rolePane(name, role)
		if err != nil {
			return err
		}
		if err := launchAgent(name, role, pane); err != nil {
			return err
		}
	}
	return nil
}

// launchAgent replaces the pane's process with the role's agent CLI. Callers
// must only pass a new pane or one whose process has already exited.
func launchAgent(name, role, pane string) error {
	prompt, err := rolePrompt(name, role)
	if err != nil {
		return err
	}
	program := agentProgram[role]
	arguments := []string{prompt}
	if role == roleReviewer {
		// The reviewer inspects the shared tree; its file editing tools are denied.
		arguments = []string{"--append-system-prompt", prompt, "--disallowedTools", reviewerDisallowedTools}
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return fmt.Errorf("%s is required for the %s but was not found in PATH", program, role)
	}
	if _, err := tmux("set-option", "-w", "-t", pane, "remain-on-exit", "on"); err != nil {
		return err
	}
	command := append([]string{"respawn-pane", "-k", "-t", pane, path}, arguments...)
	_, err = tmux(command...)
	return err
}

// respawnRole relaunches a role's agent after it exited, or re-creates a
// removed role pane next to the other role. A live pane is never replaced.
func respawnRole(name, cwd, role string) error {
	pane, exists, alive, err := registeredPane(name, role)
	if err != nil {
		return err
	}
	if alive {
		return fmt.Errorf("%s pane %s is still running; exit its process first so no work is interrupted", role, pane)
	}
	if !exists {
		peerPane, peerExists, _, err := registeredPane(name, peerRole(role))
		target := sessionTarget(name)
		if err == nil && peerExists {
			target = peerPane
		}
		pane, err = tmux("split-window", "-h", "-P", "-F", "#{pane_id}", "-t", target, "-c", cwd)
		if err != nil {
			return err
		}
		// Registration is explicit here, never inherited by an arbitrary pane.
		if _, err := tmux("set-option", "-t", sessionTarget(name), "@twomux_"+role, pane); err != nil {
			return err
		}
		if _, err := tmux("select-pane", "-t", pane, "-T", strings.ToUpper(role)); err != nil {
			return err
		}
	}
	if err := launchAgent(name, role, pane); err != nil {
		return err
	}
	fmt.Printf("Respawned %s in pane %s\n", role, pane)
	return nil
}

func printHelp() {
	fmt.Print(`2mux — Codex worker, Claude Code reviewer, automatic messages.

Usage:
  2mux                         Open or reattach; bridge starts automatically
  2mux start --agents           Create a session and launch Codex + Claude Code with role prompts
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
  2mux respawn worker|reviewer Relaunch an exited agent, or recreate its removed pane
  2mux stop                    Stop this directory's session and bridge
  2mux version                 Show version
  2mux help                    Show this help

tmux is required. Run from the project directory. --agents requires codex and claude.
Messages are delivered automatically, including while detached. Agents use '2mux send'.
`)
}
