# User guide

[Українська версія](guide.uk.md) · [Documentation](README.md) · [CLI reference](cli.md)

## Build and install

Use Linux or macOS with tmux and `ps`. Building requires Go 1.22 or later, as declared in [go.mod](../go.mod). To launch both agents automatically, install and configure the `codex` and `claude` CLIs first; 2mux uses their existing accounts, models and permission settings. A compiled 2mux binary does not require Go at runtime.

From the 2mux source checkout, build into a directory you own:

```sh
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/2mux" .
export PATH="$HOME/.local/bin:$PATH"
2mux version
2mux help
```

Add the `PATH` export to your shell configuration if you want it to persist across terminals. A normal build reports `2mux dev`. You can also build with `go build -o 2mux .` and invoke the resulting binary by its absolute path from another project.

## Launch a task

Run from the project the agents should edit:

```sh
cd /path/to/project
2mux start --agents
```

For a new session, Codex starts in the WORKER pane on the left and Claude Code in the REVIEWER pane on the right. Finish each CLI's initial trust, login or permission dialogs, then enter a concrete task in Codex. Role instructions alone tell the agents to wait; they do not start project work.

Both agents use the same directory and working tree. The worker implements the task and sends the reviewer a message starting with `READY_FOR_REVIEW`: a summary, changed files, exact validation commands and results, and open questions. The reviewer is instructed to inspect `git status` and the diff, including untracked files, and may run read-only checks or tests, but never edits files or changes git state. It replies once per request: `APPROVED` with a one-line justification, or `CORRECTIONS` as a numbered list with `file:line`, the problem and the expected fix. On a re-review it verifies the previous corrections first. The worker applies the corrections and requests another review, stops on approval without replying to it, and does not commit or push unless asked. These are agent instructions, not a filesystem access boundary; the managed and manual reviewer launches additionally deny Claude Code's `Edit`, `Write` and `NotebookEdit` tools, while shell commands remain subject to Claude Code's normal permissions.

You can also submit the task from another terminal in the project directory:

```sh
2mux send worker 'Create project documentation with verified command examples.'
```

The receipt means the task is queued. Use the worker's response and the actual changes to judge completion.

## Review messages

Agents should use the exact sending command included in their role prompt. It contains an absolute binary path, the session's private queue address and an explicit sender, so it works when an agent shell tool filters `TMUX` or `PATH`.

A worker review request uses `--kind review_request` in either tmux or native mode. Keep the generated absolute binary and queue address; the paths below are placeholders:

```sh
'/absolute/path/to/2mux' send --queue '/absolute/private/runtime' --from worker --kind review_request reviewer - <<'MESSAGE'
READY_FOR_REVIEW
Summary: added setup and CLI documentation.
Files: README.md, docs/guide.md, docs/cli.md.
Validation: checked command examples against CLI help.
Please inspect the files and reply to this exact request with APPROVED or actionable CORRECTIONS.
MESSAGE
```

For a request received through tmux, the reviewer reads the ID from its message header and sends one typed verdict, keeping its own generated binary and queue address:

```sh
'/absolute/path/to/2mux' send --queue '/absolute/private/runtime' --from reviewer --kind verdict --reply-to REQUEST_ID --verdict APPROVED worker 'APPROVED: checks passed'
```

Replace `REQUEST_ID` with the exact header ID. For corrections, use `--verdict CORRECTIONS` and a body starting with `CORRECTIONS`, listing actionable findings. For a native Claude Channel message, the reviewer uses the tools instead: `ack(message_id)` first, then `reply(reply_to, text, verdict)` with that exact ID. A legacy untyped request gets a plain reply without verdict flags. Operator reminders use ordinary `2mux send reviewer 'Please check the worker request.'`; they do not create a typed review request.

Typed review correlation applies to every transport. Finish edits and validation before sending: the scope includes the whole Git working tree, so even an unrelated file change can invalidate the verdict. Each request accepts one verdict; after `CORRECTIONS`, fix the issues, rerun validation and send a new request. Stop after `APPROVED` without replying to it. Review approval does not authorize tool execution, commit or push.

Quoted heredoc delimiters preserve literal text, including quotes, backticks and dollar signs. Merely printing review markers does not send a message. Each `send` invocation creates a distinct message; repeated requests have distinct IDs.

## Verify peer communication

After completing startup dialogs, send one diagnostic note using the worker's generated command:

```sh
'/absolute/path/to/2mux' send --queue '/absolute/private/runtime' --from worker --kind note reviewer - <<'MESSAGE'
COMMUNICATION_CHECK
Reply once with COMMUNICATION_OK to this exact message ID; no review verdict is needed.
For a Claude Channel message, call ack first, then reply without a verdict.
MESSAGE
```

Inspect `2mux status --json --queue '/absolute/private/runtime'` and the corresponding `messages/ID.json` files. The reviewer response must come from `reviewer`, target `worker` and contain the exact diagnostic ID in `reply_to`. Through tmux, the reviewer can add `--kind note --reply-to MESSAGE_ID` to its generated send command; through Channel, it uses `reply` without `verdict`. A `queued` receipt or `channel_connected: true` alone does not complete this check. Confirm the actual response in the worker pane too.

In a mixed `--claude-channel` session, the outbound note can be `accepted` through Channel while the return note waits for the worker's tmux pane to become quiet. `worker: unknown` can mean that no native state observer is enabled. Let the worker finish its current output and check the existing receipts before sending another note. This diagnostic checks delivery and correlation; the subsequent review needs its own `review_request` and verdict. It does not verify the separate Codex API transport or tool approval routing.

## Visible communication in the CLI

Incoming messages start with a compact card identifying the sender, recipient and message type. Exact receipt IDs and the closing boundary remain available below the card for correlation. Outgoing `send` and Claude Channel `ack`/`reply` results show the same layout with `queued` or `accepted`, plus a preview of up to eight lines and 480 characters. The complete message stays in the queue and is delivered without truncation:

```text
╭─ 2mux · WORKER → REVIEWER
│ REVIEW REQUEST · queued
╰──────────────────────────────────────
READY_FOR_REVIEW
Summary: make peer communication visible in both CLIs.
Validation: transport and display checks passed.
```

Role prompts and Channel instructions ask each agent to show a short entry in its normal conversation when receiving a message and before sending a request or verdict. Native CLIs control how notifications and tool results are collapsed; the conversation entry keeps the exchange visible without opening those details. An approval is reported once to the user and is never acknowledged back to the peer.

Pane headers use cyan for WORKER/Codex and violet for REVIEWER/Claude. They show the latest related message's direction, receipt state and type even while the agent is busy. On panes narrower than 60 columns, role labels shorten and peers become `W`, `R` or `U`. For example, `WORKER │ ←R · queued · approved` means the latest reviewer verdict is queued, not that worker delivery has completed. Headers summarize the latest message, not approval of the current working tree. Empty panes show who they are waiting for; a corrupt queue shows `Queue needs attention`. The read-only header job never starts a model turn or includes message bodies.

After rebuilding, run `2mux start --detach` to apply the headers to an existing pair without restarting agents. Header polling uses tmux's existing status refresh interval. Already-running bridge and Channel processes keep the message formatter loaded at launch; the new incoming cards and native tool receipts take effect when those processes next start. Fresh role prompts contain the conversation-display instruction; for an existing agent, send a note asking it to show peer exchanges in its normal conversation.

## Detach and return

With the default tmux prefix, press `Ctrl+B`, then `D` to detach. The agents and bridge continue running. From the same project directory, run:

```sh
2mux
```

This reattaches to the existing session and starts the bridge if it has stopped. Inside a tmux client it switches that client to the session. Role assignments use stored pane IDs, so swapping panes or adding a window does not change the recipients.

For a launch without attaching:

```sh
2mux start --agents --detach
2mux status
```

Attach later with `2mux` to finish any initial agent dialogs. Detached launch does not handle those dialogs for you.

## Launch agents manually

`2mux start` creates two shells with the bridge running. In the WORKER pane:

```sh
exec codex "$(2mux prompt worker)"
```

In the REVIEWER pane:

```sh
exec claude --append-system-prompt "$(2mux prompt reviewer)" --disallowedTools "Edit Write NotebookEdit"
```

Run these from the project directory with `2mux` in `PATH`. `exec` replaces the shell, preventing a later delivery from entering that shell after the agent exits. Regenerate role prompts after recreating a session because the queue address changes.

On an existing session, `start --agents` checks agent presence and does not replace the current processes. If the panes still contain shells, launch manually as above. If an agent exited and left a dead pane, or its pane was removed, run `2mux respawn worker` or `2mux respawn reviewer`; queued messages for that role are delivered once it runs.

## Diagnose delivery

Start with:

```sh
2mux status
2mux messages
```

`status` reports registered panes, detectable agents, bridge health, message counts and the runtime directory. `messages` lists IDs, recipients, states, errors and the message-record directory. It does not print message bodies; individual JSON records contain them.

| Symptom | What to do |
| --- | --- |
| `tmux ... not found in PATH` | Make tmux available to the launching shell and retry. |
| `codex` or `claude` is required | Make both configured CLIs available in `PATH` before `start --agents`. |
| Pane is waiting for an interactive agent | Complete manual launch or inspect the foreground program. Messages wait while a shell or an unrecognized process is present. |
| Pane is in copy mode or has input disabled | Exit copy mode or restore pane input, then check the queue again. |
| Bridge is stopped, or an agent's send prints `Warning: 2mux bridge is ...` | Run `2mux start --detach` from the same directory to restart it without attaching. Queued messages are then delivered. |
| Bridge is locked but unresponsive | Inspect `status`, `health.json` and `bridge.log` in the reported runtime directory. A held lock is not bypassed by starting another bridge. |
| A registered pane is missing/dead | Save any work, then run `2mux respawn ROLE` to relaunch that agent. `2mux` still reattaches meanwhile. |
| Pane shows a confirmation dialog | Answer the agent's dialog yourself; held messages follow once it closes. |
| Waiting for a pane to become idle | Normal while an agent is producing output; delivery follows after one quiet second. |
| `status` lists corrupt records | Inspect the named file in the `messages` directory, then fix or remove it; delivery is paused until then. |
| The session has no bridge runtime | Save work, then use `2mux stop` and create a new session. |
| A same-name session is not owned by 2mux | Inspect that tmux session separately; 2mux refuses to attach to or stop it. |
| A message is `uncertain` | Inspect the receiving agent before resolving the receipt, as described below. |

The bridge checks for a recognizable foreground agent, a quiet screen and known confirmation phrases, not whether every CLI dialog is ready for a prompt. Finish native confirmations yourself and avoid typing in the receiving pane while a message is being submitted.

## Resolve an uncertain message

`delivered` means the bridge pasted the message and submitted Enter to the terminal. It does not confirm that a model read or completed the request. If submission may have begun before an error or crash, the bridge marks the message `uncertain` and pauses later messages for that recipient.

1. Find its ID and error with `2mux messages`.
2. Inspect the receiving pane and any agent response for that message ID.
3. If submission is confirmed, replace the placeholder ID and mark it delivered:

   ```sh
   2mux resolve MESSAGE_ID delivered
   ```

4. If you have established that it was not submitted, requeue the same record:

   ```sh
   2mux resolve MESSAGE_ID retry
   ```

Keep the receipt unresolved while the outcome is unknown. A retry can duplicate a request that was already submitted. The other recipient can continue receiving its messages.

## Stop and retain records

Save work in both panes, then run from the project directory:

```sh
2mux stop
```

This stops the bridge and tmux session, ending the pane processes. Independent CLI services manage their own lifecycle. The printed runtime directory remains available for inspecting or manually removing records. Save any needed receipts before temporary storage is cleared by the operating system.

Records contain message text. Private directory/file permissions limit access by other Unix users; both agents still share your account and project. 2mux does not collect terminal transcripts, credentials or telemetry. Use a new role prompt and queue address after creating a replacement session.

## Experimental native communication

Save work before replacing an existing session. From the same project root, create a new session with `2mux start --native`, or use `--codex-api` / `--claude-channel` independently. Each flag implies `--agents`. Reattachment cannot change transports or interrupt an agent. The Codex backend runs in the session's separate `2mux-api` window; stopping the session stops that private backend. Shared Codex daemons are not touched.

Finish Codex startup dialogs in the worker pane. Claude's `--dangerously-load-development-channels server:twomux` displays a fullscreen consent dialog at every launch. Confirm that dialog and any MCP consent yourself, then verify the native Claude startup notice lists `server:twomux` as a Channel. If the account/provider/organization policy rejects Channels, do not send: save work and recreate without `--claude-channel`. An MCP connection reported by 2mux alone cannot verify that policy.

Enter the task in the worker pane and use the [review message workflow](#review-messages) above. The worker still sends to its private queue; native Claude Channel instructions ask the reviewer to call `ack(message_id)` first, then `reply(reply_to, text, verdict)` for the exact request. IDs are never inferred from the latest message.

Git review scope combines HEAD, staged/unstaged diff and non-ignored untracked paths/content. Changes after the request invalidate its verdict at enqueue and again before delivery. Correlated request/reply records are retained for validation. Outside a Git repository (or before its first commit), scope is absent; exact ID and role checks still apply.

Use `2mux status --json` or `2mux watch` for role states, counts and per-role `transports`. Full native mode requires `transports.worker: "codex"` and `transports.reviewer: "claude-channel"`; a mixed pair with worker `tmux` is not full native mode. Native queue submission is prompt, but ordinary queued input waits for a turn boundary; `--steer` is the explicit option for active-turn input. `starting` is not idle; approvals and user-input waits are separate from review verdicts. Managed Claude hooks observe busy/idle/approval/exited events. Codex events are filtered by the session's exact thread. The session status line shows both roles.

On reconnect Codex reads the existing queue and paginated user-item history. An exact queue ID remains `submitted`; an exact user-item client ID becomes `accepted`; an absent ID becomes `uncertain`. Claude Channel restart cannot prove an unacknowledged send, so it becomes `uncertain` and later messages wait until explicit ack or operator resolution. There is no automatic resend through paste or another transport after an attempt. Native outages remain visible; `2mux start` reconnects to an existing backend, and `respawn` resumes the saved agent session after its pane exits.

New Codex worker threads are named before the bootstrap connection closes, which persists their rollout without starting a model turn. If Codex reports `no rollout found` (`-32600`), bootstrap and bridge clear the saved ID before creating and saving a replacement. Other resume errors and thread/CWD mismatches stop recovery without replacing the thread. After an exited worker's missing-rollout crash, run `2mux respawn worker`; the preflight repairs its saved thread before launching the TUI.

`resolve ID delivered` is an operator assertion that submission occurred, not a native receipt. `resolve ID retry` explicitly authorizes another attempt using the same recorded transport. Check the native queue/history and recipient first. An unsupported native CLI version returns an error without changing transports or replaying receipts. Select legacy transport explicitly when creating a new session if you need it.

An unacknowledged Channel submission becomes `uncertain` after 90 seconds, even without a restart; late ack can still confirm it. A stale pending verdict becomes terminal `rejected` with its reason and is never rechecked or submitted automatically.
