// A deterministic local TUI fixture. No model or network calls are made.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	role := "worker"
	if filepath.Base(os.Args[0]) == "pi" {
		role = "reviewer"
	}
	dir := os.Getenv("TWOMUX_FIXTURE_DIR")
	binary := os.Getenv("TWOMUX_BINARY")
	if dir == "" || binary == "" {
		panic("fixture environment missing")
	}
	stty := exec.Command("stty", "raw", "-echo")
	stty.Stdin = os.Stdin
	if err := stty.Run(); err != nil {
		panic(err)
	}
	fmt.Print("\x1b[?2004h")
	if err := os.WriteFile(filepath.Join(dir, role+".ready"), []byte("ready"), 0600); err != nil {
		panic(err)
	}
	var prompt []byte
	var incoming []byte
	inPaste := false
	reviews := 0
	for {
		b := make([]byte, 4096)
		n, err := os.Stdin.Read(b)
		if err != nil {
			return
		}
		incoming = append(incoming, b[:n]...)
		for len(incoming) > 0 {
			if incoming[0] == 27 {
				if len(incoming) < 6 {
					break
				}
				switch string(incoming[:6]) {
				case "\x1b[200~":
					inPaste = true
				case "\x1b[201~":
					inPaste = false
				default:
					panic(fmt.Sprintf("unexpected escape: %q", incoming))
				}
				incoming = incoming[6:]
				continue
			}
			c := incoming[0]
			incoming = incoming[1:]
			if c != '\r' || inPaste {
				prompt = append(prompt, c)
				continue
			}
			text := string(prompt)
			prompt = nil
			f, err := os.OpenFile(filepath.Join(dir, role+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				panic(err)
			}
			if err := json.NewEncoder(f).Encode(text); err != nil {
				panic(err)
			}
			f.Close()
			peer, reply := "reviewer", ""
			if role == "worker" && (strings.Contains(text, "TASK:") || strings.Contains(text, "CORRECTIONS:")) {
				reply = "READY_FOR_REVIEW:\nbridge.go changed; tests pass.\nПеревір багаторядкове повідомлення."
			}
			if role == "reviewer" && strings.Contains(text, "READY_FOR_REVIEW:") {
				peer = "worker"
				reviews++
				reply = "CORRECTIONS:\nFix the retry case.\nПеревір UTF-8."
				if reviews > 1 {
					reply = "APPROVED"
				}
			}
			if reply != "" {
				cmd := exec.Command(binary, "send", peer, "-")
				cmd.Stdin = bytes.NewBufferString(reply)
				if output, err := cmd.CombinedOutput(); err != nil {
					panic(fmt.Sprintf("send: %s: %v", output, err))
				}
			}
		}
	}
}
