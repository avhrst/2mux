# User guide

[Українська версія](guide.uk.md) · [Documentation](README.md) · [CLI reference](cli.md)

## Build and install

Use Linux or macOS with tmux and `ps`. Building requires Go 1.22 or later, as declared in [go.mod](../go.mod). To launch both agents automatically, install and configure the `codex` and `pi` CLIs first; 2mux uses their existing accounts, models and permission settings. A compiled 2mux binary does not require Go at runtime.

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

For a new session, Codex starts in the WORKER pane on the left and pi in the REVIEWER pane on the right. Finish each CLI's initial trust, login or permission dialogs, then enter a concrete task in Codex. Role instructions alone tell the agents to wait; they do not start project work.

Both agents use the same directory and working tree. The worker implements the task and sends the reviewer a summary, changed files and test results. The reviewer is instructed to inspect files and relevant tests without editing project files, then return `CORRECTIONS` or `APPROVED`. The worker applies actionable corrections and requests another review, ending the cycle on approval. These are agent instructions, not a filesystem access boundary.

You can also submit the task from another terminal in the project directory:

```sh
2mux send worker 'Create project documentation with verified command examples.'
```

The receipt means the task is queued. Use the worker's response and the actual changes to judge completion.

## Review messages

Agents should use the exact sending command included in their role prompt. It contains an absolute binary path, the session's private queue address and an explicit sender, so it works when an agent shell tool filters `TMUX` or `PATH`.

An operator in the project directory can send a message with the short form:

```sh
2mux send reviewer - <<'MESSAGE'
Ready for review.
Summary: added setup and CLI documentation.
Files: README.md, docs/guide.md, docs/cli.md.
Validation: checked command examples against CLI help.
Please inspect the files and reply with APPROVED or actionable CORRECTIONS.
MESSAGE
```

Quoted heredoc delimiters preserve literal text, including quotes, backticks and dollar signs. Merely printing review markers does not send a message. Each `send` invocation creates a distinct message; repeated requests have distinct IDs.

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
exec pi --append-system-prompt "$(2mux prompt reviewer)"
```

Run these from the project directory with `2mux` in `PATH`. `exec` replaces the shell, preventing a later delivery from entering that shell after the agent exits. Regenerate role prompts after recreating a session because the queue address changes.

On an existing session, `start --agents` checks agent presence and does not replace the current processes. If the panes still contain shells, launch manually as above. If a registered pane is dead or missing, save work in surviving panes and recreate the session.

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
| `codex` or `pi` is required | Make both configured CLIs available in `PATH` before `start --agents`. |
| Pane is waiting for an interactive agent | Complete manual launch or inspect the foreground program. Messages wait while a shell or an unrecognized process is present. |
| Pane is in copy mode or has input disabled | Exit copy mode or restore pane input, then check the queue again. |
| Bridge is stopped | Run `2mux start --detach` from the same directory to restart it without attaching. |
| Bridge is locked but unresponsive | Inspect `status`, `health.json` and `bridge.log` in the reported runtime directory. A held lock is not bypassed by starting another bridge. |
| A registered pane is missing/dead, or the session has no bridge runtime | Save work, inspect delivery records if available, then use `2mux stop` and create a new session. |
| A same-name session is not owned by 2mux | Inspect that tmux session separately; 2mux refuses to attach to or stop it. |
| A message is `uncertain` | Inspect the receiving agent before resolving the receipt, as described below. |

The bridge checks for a recognizable foreground agent, not whether every CLI dialog is ready for a prompt. Finish native confirmations yourself and avoid typing in the receiving pane while a message is being submitted.

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
