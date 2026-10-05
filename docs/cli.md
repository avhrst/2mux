# CLI reference

[Українська версія](cli.uk.md) · [Documentation](README.md) · [User guide](guide.md)

## Command context

Project commands address the session for the current working directory, with symlinks resolved. Run them from the project root used at startup, rather than a subdirectory. `help`, `version`, `send --queue DIR`, `status --queue DIR --json` and `watch --queue DIR` do not require tmux or project-session discovery. Other commands require tmux in `PATH`.

Recipient and sender roles are case-sensitive: `worker` or `reviewer`. Start flags take no values. Send options use separate arguments, precede the recipient and take nonempty values (except the boolean `--steer`); forms such as `--queue=DIR` are not supported. An empty `--queue` value is rejected rather than falling back to project discovery.

`start` and `stop` serialize session changes for the same canonical project directory, waiting up to ten seconds for the lifecycle lock. The lock namespace is independent of caller `TMPDIR`. Interactive attachment releases that lock, so another terminal can still reattach or stop the session.

Successful commands exit with status `0`; an error prints `Error: ...` to stderr and exits `1`. With no session, `status`, `messages` and `stop` print that 2mux is not running and return success. `send` without a queue address, `prompt`, `respawn` and `resolve` instead return an error.

## Start and attach

```text
2mux
2mux start [--agents] [--detach] [--native|--codex-api|--claude-channel]
```

| Option | Behavior |
| --- | --- |
| No options | Create two shells or reattach to an existing session; ensure the bridge is running. |
| `--agents` | In a new session, launch Codex as worker and Claude Code as reviewer with generated role instructions; the reviewer runs with `--disallowedTools "Edit Write NotebookEdit"`. Requires the `codex` and `claude` CLIs in `PATH`. In an existing session, check agent presence without replacing processes. |
| `--detach` | Create/reuse the session and ensure the bridge is running, then print its name without attaching. Can be combined with `--agents`. |

An interactive launch attaches through tmux; when already in a tmux client, it switches that client. Unknown start options are errors. Existing sessions must have the matching directory marker and a valid private runtime directory. A missing or dead role pane prints a warning but does not prevent attaching; it is never replaced automatically. Use `2mux respawn ROLE` to relaunch it.

## Send a message

```text
2mux send [--queue DIR] [--from ROLE] [--kind KIND] [--reply-to ID] [--verdict VERDICT] [--steer] <worker|reviewer> <message|->
```

| Argument | Behavior |
| --- | --- |
| Recipient | Required: `worker` or `reviewer`. |
| Message | Exactly one shell argument, or `-` to read stdin. Quote text containing spaces. |
| `--queue DIR` | Address an existing private queue directly through filesystem I/O. Both `DIR` and `DIR/messages` must be absolute directories owned by the current Unix user with mode `0700`. This path does not discover the tmux session, check the recipient pane or start/recover the bridge. If no healthy bridge holds the queue, the message is still queued and a warning is printed to stderr. |
| `--from ROLE` | Set the sender to `worker` or `reviewer`. This is a message label, not authentication. |
| `--kind KIND` | `note` (ordinary message), `review_request` or `verdict`. A review request must come from an agent role and target its peer; use explicit `--from` for agent commands. |
| `--reply-to ID` | Exact existing opposite-role message ID. Required for a verdict; optional for a correlated note. Read the received header instead of guessing the latest request. |
| `--verdict VERDICT` | `APPROVED` or `CORRECTIONS`, only with `--kind verdict` and an exact `review_request` target. Each request permits one verdict. |
| `--steer` | Steer an active worker turn in a `--codex-api` session. Rejects other recipients, non-native sessions and sessions without an active turn. |

Without `--queue`, 2mux finds the current directory's session, checks that the recipient pane exists and ensures the bridge is running. The sender is inferred from `TMUX_PANE` when it matches a registered role, otherwise it is `user`. With `--queue`, the sender defaults to `user`. `--from` overrides either default; the literal value `user` is not accepted by that option.

On success, the command prints `Queued message: ID`, followed by a compact role card and text preview. This confirms persistence in the queue, not delivery or task completion.

Message rules:

- Nonempty after trimming whitespace, valid UTF-8 and at most 65,536 bytes, including newline bytes.
- Multiline text and tabs are accepted. Other Unicode control characters, including carriage return and Escape, are rejected; use LF line endings.
- Bidirectional control characters are rejected, and so is any line whose first non-blank text is `[2mux` or `╭─ 2mux ·`, which could imitate a protocol header or role card.
- Text is delivered between a header containing the message ID, sender and recipient and an end marker `[2mux end of message ID]`.

Operator example, run from the project directory:

```sh
2mux send reviewer - <<'MESSAGE'
Please check the worker review request.
Files: docs/guide.md, docs/cli.md.
Checks: command syntax and local links verified.
MESSAGE
```

Agent example with placeholder paths (use the exact command from `2mux prompt worker`):

```sh
'/absolute/path/to/2mux' send --queue '/absolute/private/runtime' --from worker --kind review_request reviewer - <<'MESSAGE'
READY_FOR_REVIEW
Changes, files, exact validation commands/results and known limitations.
MESSAGE
```

Typed requests and verdicts work through every transport, including default tmux delivery. The request snapshots the whole Git scope, not just the files named in its body; subsequent edits can invalidate a verdict. Finish edits and validation first. A diagnostic `--kind note` is not a review request. See the [review workflow](guide.md#review-messages) for exact verdict commands, Channel `ack`/`reply`, legacy replies and rereview after corrections.

## Print role instructions

```text
2mux prompt <worker|reviewer>
```

Requires a running project session and valid runtime metadata. Prints the role instructions plus the invoking binary's absolute path and this session's queue address. It does not launch an agent or send a message. The worker waits for a concrete task and requests review with `READY_FOR_REVIEW`; the reviewer waits for a worker message and replies once with `APPROVED` or numbered `CORRECTIONS`. Generate a new prompt after recreating a session.

## Inspect status and receipts

```text
2mux status [--json] [--queue DIR]
2mux watch [--queue DIR]
2mux messages
```

Plain `status` prints session name, canonical project directory, each role's pane and readiness result, bridge health, counts by message state and the runtime directory. `status --json` reports reduced role states, session config, per-role `transports`, receipt counts, delivery errors and Channel connectivity. Plain status also prints each role's configured transport. `transports.worker: "tmux"` means Codex API is disabled, even if the reviewer uses a native Channel. With `--queue DIR`, it reads that private runtime directly; even without `--json`, this form prints JSON and does not discover tmux panes. `watch` emits changed JSON snapshots once per second. `messages` prints each message's ID, sender, recipient, state and any delivery error, followed by the `messages` directory path; it has no `--queue` option, so use direct JSON record inspection when tmux context is unavailable. None of these commands starts a stopped bridge.

| State | Meaning | Next step |
| --- | --- | --- |
| `queued` | Persisted and awaiting delivery. A pre-paste error can leave it queued with an error. | Inspect pane/bridge state; queued deliveries are attempted automatically while the bridge runs. |
| `sending` | The bridge recorded a delivery attempt before interacting with the terminal. | A brief normal state. After bridge interruption, recovery changes it to `uncertain`. |
| `delivered` | Paste and Enter succeeded, or an operator marked the receipt delivered. | Check the agent's response and task results separately. |
| `uncertain` | A paste/submission may have happened, or the bridge stopped during an attempt. | Inspect the receiving agent, then resolve explicitly. Later messages to this recipient wait. |

Bridge states are `running (PID ...)`, `stopped`, `unresponsive` or `error: ...`. A running bridge may also report the most recent queue/delivery error, including `waiting for pane ... to become idle` and `pane ... shows a confirmation dialog`. The bridge only pastes into a pane whose screen has been unchanged for one second and holds messages while known confirmation phrases are visible near the bottom of the screen. This detection is heuristic; finish native CLI dialogs yourself.

If a record file is corrupt, `status` and `messages` still list the valid records and name each corrupt file. Delivery pauses for every recipient until the file is fixed or removed from the `messages` directory.

## Resolve an interrupted delivery

```text
2mux resolve <message-id> <delivered|retry>
```

Use the full message ID from `messages`, after inspecting the receiving agent:

- `delivered` marks the record delivered and clears its error.
- `retry` changes the same record to `queued` and clears its error. The running bridge can then deliver it again.

Only `uncertain` or interrupted `sending` records can be resolved. Unknown IDs and other states are errors. Resolution serializes with bridge delivery; a busy queue can ask you to try again. `resolve` does not start a stopped bridge. Use `2mux start --detach` if needed. Do not retry while the outcome remains unknown.

## Respawn an exited agent

```text
2mux respawn <worker|reviewer>
```

Relaunches the role's agent CLI (Codex for the worker, Claude Code for the reviewer) with fresh role instructions. If the registered pane is dead, for example after the agent exited, the agent restarts in that pane. If the pane was removed, a new pane is split next to the other role and registered explicitly. A live pane is never replaced: exit its process first. Messages queued for the role are delivered once the agent is running.

## Stop

```text
2mux stop
```

Save pane work first. Stops the current directory's bridge and tmux session, ending pane processes. Delivery records are retained, and their runtime path is printed. A new session receives a new runtime directory; the old queue is not transferred automatically. Independent agent CLI services manage their own lifecycle.

The directory ownership marker must match before a session can be stopped. An older owned session without bridge metadata can still be stopped.

## Help and version

```text
2mux help
2mux -h
2mux --help
2mux version
2mux -v
2mux --version
```

Aliases are top-level commands, not start options. Help and version accept no additional arguments. Normal builds report `2mux dev`; the version variable is defined in [version.go](../version.go).

`_bridge` is an internal child-process entry point. Start or recover it through `start`, rather than calling it directly.

## Native options and correlation

| Option | Meaning |
| --- | --- |
| `start --codex-api` | In a new session, launch both agents with Codex on a private app-server and remote TUI. |
| `start --claude-channel` | In a new session, launch both agents with Claude on an MCP Channel. Research preview; the user confirms development-channel and MCP consent. |
| `start --native` | Enable both experimental transports. |
| `send --kind KIND` | `note`, `review_request`, `verdict`. Review requests/verdicts require an agent role via `--from`. |
| `send --reply-to ID` | Exact existing ID from the opposite role; mandatory for verdicts. |
| `send --verdict VERDICT` | `APPROVED` or `CORRECTIONS`, only with `--kind verdict`. Duplicate/stale verdicts are rejected. |
| `send --steer` | Explicitly steer an active worker turn in a `--codex-api` session. |
| `status --json [--queue DIR]` | JSON role states, counts, config and delivery errors. A private queue address works without tmux. |
| `watch [--queue DIR]` | Print changed JSON status snapshots once per second until Ctrl+C. |

| State | Meaning |
| --- | --- |
| `submitted` | Native transport write confirmed, agent receipt not yet confirmed. |
| `accepted` | Codex user item has the exact client ID or Claude called `ack`; hooks can also acknowledge a legacy message. |
| `rejected` | Invalid review verdict or Git scope changed before delivery; the reason remains in its record. No automatic retry. |

Native options conservatively check the CLI version. Validated versions: Codex 0.159.2 / 0.160.0 and Claude 2.1.289. Unsupported versions fail before creating a new session; native transports never fall back to tmux. Existing sessions and attempted receipts are preserved on a version mismatch. Codex prints an explicit warning while live approval-routing gate G2 remains unverified. A missing Channel ack becomes `uncertain` after 90 seconds; a later explicit ack can still confirm it.

`channel_connected` only proves MCP connectivity. Check `server:twomux` registration in the native TUI. `starting`, `busy`, `idle`, `awaiting_approval`, `awaiting_input`, `exited`, `unknown` describe observed role state, not review outcome. Legacy hooks observe; a fresh approval holds paste for at most 15 seconds, while pane/dialog checks remain active.

Legacy Codex workers also report `busy`, `idle` and supported `awaiting_approval` screens with `source: "codex-tui"`, based on explicit controls in their registered pane. This display heuristic works without `--codex-api`; unfamiliar layouts or input dialogs remain `unknown`, as do copy mode and failed observations. The status bar refreshes once per second. Existing sessions need the rebuilt bridge restarted to acquire this observer; rebuilding the binary alone does not replace a running bridge. Native Codex API events remain the authoritative source when that transport is enabled.

Internal `_hook` and `_channel` are not public operator commands. They run through private session settings/MCP configuration only. See the [guide](guide.md#experimental-native-communication) for exact request/verdict and recovery examples.
