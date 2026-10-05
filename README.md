# 2mux

[Українська версія](README.uk.md)

2mux pairs a **Codex WORKER** and a **Claude Code REVIEWER** in two tmux panes in the same project. A background bridge automatically delivers messages between the agents, including while you are detached. Agents send through `2mux send`; you do not have to copy feedback between panes.

## Documentation

Start with the [user guide](docs/guide.md) for installation, a complete review workflow and troubleshooting. The [CLI reference](docs/cli.md) covers every public command, and [architecture and development](docs/architecture.md) explains the queue, bridge and test suites. See the [documentation index](docs/README.md) for both languages.

## Build and start

Requirements: Linux or macOS, tmux, and Go 1.22+ to build. `--agents` also requires the `codex` and `claude` CLIs, already configured with your preferred accounts and settings. A built binary does not need Go.

```sh
go build -o 2mux .
./2mux version
```

Use the binary's absolute path or install it into a directory in your `PATH`. From the project you want the agents to work on:

```sh
cd /path/to/project
2mux start --agents
```

On a new session this launches Codex on the left and Claude Code on the right, with role instructions and the absolute path to the message command. Finish any initial CLI permission dialogs, then enter your task in Codex. The worker sends a `READY_FOR_REVIEW` request when ready; Claude Code sends `CORRECTIONS` or `APPROVED` back automatically. Both agents share the working tree; the reviewer is instructed to inspect it without editing, and the managed launch denies Claude Code's file-editing tools (`Edit`, `Write`, `NotebookEdit`). Their normal permission and account settings still apply, including Claude Code's permission prompts for shell commands.

Detach with `Ctrl+B`, then `D`. Run `2mux` again from the same directory to reattach. Inside an existing tmux client, 2mux switches that client to the session. Pane roles survive swaps and changes to the active window.

`2mux` without `--agents` creates ordinary shells with the bridge enabled. If you prefer to launch agents yourself, run these in their respective panes:

```sh
# WORKER pane
exec codex "$(2mux prompt worker)"

# REVIEWER pane
exec claude --append-system-prompt "$(2mux prompt reviewer)" --disallowedTools "Edit Write NotebookEdit"
```

`--agents` only launches processes in a new session. On reattachment it checks that both agents are present and does not replace existing processes. If an older shell session exists, launch agents manually or save work and recreate it with `2mux stop`.

## Automatic messages

Role prompts give each agent an exact command with an absolute binary path, `--queue DIR` and `--from ROLE`. This address uses filesystem I/O and survives shell tools that filter `TMUX`, `TMUX_PANE` or `PATH`. Use the generated command when sending from an agent. An operator in the project directory can use the shorter form:

```sh
2mux send reviewer - <<'MESSAGE'
Ready for review: implemented the change.
Files: bridge.go, queue.go.
Validation: go test -race ./... passed.
MESSAGE
```

The reviewer replies using `2mux send worker`. An operator can also send a task or message with `2mux send worker "Your task"`. Message bodies can contain multiple lines and UTF-8 text, up to 64 KiB. Terminal and bidirectional control characters are rejected, as are lines starting with `[2mux`, so a body cannot imitate a message header. Printing `<READY_FOR_REVIEW>` or `<CORRECTION>` alone does not send anything; the transport is the command, independent of terminal output and redraws.

The bridge records each message before delivering it and pastes it as a single prompt between a header and an end marker, then submits it. It uses tmux's [bracketed paste support](https://man.openbsd.org/tmux.1#paste-buffer). Delivery waits while the target pane is a shell, is dead, is in copy mode, has disabled input, or is not running an identifiable Codex/Claude Code foreground process. It also waits until the pane's screen has been unchanged for one second and while a confirmation dialog (such as a Codex command approval or a Claude Code permission prompt) is visible, so Enter never answers that dialog. Managed launches and the manual `exec` examples leave no shell behind when an agent exits. Answer native CLI confirmations yourself. Avoid typing in a pane while incoming messages are being submitted.

`queued` means accepted into the local queue. `delivered` means submitted to the terminal; it is **not** proof that the model received, understood, or completed the request. The receiving CLI controls how prompts are queued while it is busy.

## Commands

| Command | Effect |
| --- | --- |
| `2mux` / `2mux start` | Create or reattach; start or recover the bridge. |
| `2mux start --agents` | On a new session, launch Codex + Claude Code with role prompts. |
| `2mux start --detach` | Start without attaching; can be combined with `--agents`. |
| `2mux send worker\|reviewer "text"` | Queue a message; use `-` to read stdin. |
| `2mux prompt worker\|reviewer` | Print instructions with this running session's queue address. |
| `2mux status` | Show registered panes, agent presence, bridge health and message counts. |
| `2mux messages` | List receipts, errors and the private record directory. |
| `2mux resolve ID delivered` | After checking the peer, mark an uncertain delivery as submitted. |
| `2mux resolve ID retry` | After checking the peer, requeue an uncertain delivery. |
| `2mux respawn worker\|reviewer` | Relaunch an agent that exited, or recreate its removed pane. |
| `2mux stop` | Stop this directory's bridge, session and its processes. |
| `2mux help` / `2mux version` | Show help / version (`dev` for a normal build). |

## Recovery and storage

Only one bridge can hold a session's process lock. Messages are persisted as atomic JSON records in a private temporary directory (mode `0700`; files `0600`) printed by `status`. A bridge restart continues queued deliveries without replaying completed ones. Delivery order is preserved for each recipient. An unavailable recipient does not block the other direction. Delivered records older than a day move to `messages/archive/`. If the bridge stops, an agent's `send` still queues the message and prints a warning until the operator restarts it with `2mux start`.

Starts and stops for the same canonical project directory are serialized, including the first session creation. The fixed private `/tmp/2mux-control-UID` directory retains the project lock files independently of the caller's `TMPDIR`; it contains no message bodies. The lifecycle lock is released before interactive attachment.

If the bridge crashes during submission, or tmux returns an error after a paste may have started, the receipt becomes `uncertain`. Later messages for that recipient wait. Check the receiving agent first, then use `resolve` to mark the receipt delivered or retry it. An uncertain message is never silently retried.

`stop` ends pane processes, so save their work first. Independent CLI services manage their own lifecycle. It keeps delivery records for inspection; remove the printed directory when no longer needed. Each new session gets a fresh private directory. These records contain exchanged message bodies; terminal transcripts, account credentials and telemetry are not collected. Temporary storage is not a permanent archive and may be cleared by the operating system.

Session names include a hash of the canonical project path. 2mux verifies the directory marker before attaching or stopping a session and uses stored pane IDs rather than screen positions. An unowned session with a conflicting name is rejected. A missing/dead registered pane produces a warning but does not prevent reattaching; its messages wait until `2mux respawn ROLE` relaunches the agent. Another pane never silently takes over a role. Sessions from the earlier shell-only implementation have no bridge metadata: save work, stop them and create a new session.

## Validation

```sh
go test -race ./...
go vet ./...
TWOMUX_INTEGRATION=1 go test -race -v ./...
TWOMUX_NATIVE_SMOKE=1 go test -v -run TestInstalledAgentPresence
```

The opt-in integration test uses a separate tmux socket and deterministic local TUI fixtures. It exercises concurrent first starts with shells and agents, the complete worker → reviewer → corrections → rereview → approval cycle, multiline UTF-8, detach, pane swaps, extra windows, bridge recovery, duplicate prevention, copy mode, disabled input, confirmation dialogs, shell safety, exited-agent respawn and session ownership. It makes no model or network calls and does not use your tmux sessions. CI runs it on Linux and macOS with Go 1.22 and the current stable release. The separate native smoke test starts installed Codex and Claude Code without submitting a model prompt and checks their process identification. Real model responses and CLI prompt queue behavior require testing with your installed agents.

An earlier Codex + pi pair, before the reviewer switched to Claude Code, also completed a live automatic file-review scenario; see the [review findings](REVIEW.md) and [receipts](validation/live-review-20260930.json).

2mux uses the Go standard library, tmux and `ps`. See [LICENSE](LICENSE).
