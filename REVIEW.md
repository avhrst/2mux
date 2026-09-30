# 2mux review — 2026-09-30

[Українська версія](REVIEW.uk.md)

Reviewed all original source, session creation and ownership, pane roles, message transport, recovery, CLI, tests and documentation. The main cause was missing functionality: the original version opened two shells and never called the bridge helpers. The automatic exchange expected by the user was not implemented.

## Findings and fixes

| Priority | Problem and impact | Implemented fix |
| --- | --- | --- |
| P1 | `start` only attached tmux; agents received no messages from each other. | An automatic background bridge delivers queued messages while detached. Added `start --agents` for Codex + pi with role instructions. |
| P1 | A live test revealed that Codex's shell tool filtered `TMUX`/`PATH`, so a short `send` could not find the correct session. | Generated instructions include the absolute binary path, explicit `--queue DIR` and `--from ROLE`. This sending path uses only filesystem I/O and does not need tmux inside the agent's shell tool. |
| P1 | There was no queue or delivery receipt. Screen-capture helpers could not provide recovery or replay prevention. | Atomic JSON records, unique IDs, per-recipient ordering, process locks and `queued`, `sending`, `delivered`, `uncertain` states. Completed deliveries are not replayed after restart. |
| P1 | An ambiguous paste/submission outcome could cause duplicate delivery. | `uncertain` blocks later messages to that recipient until explicit resolution. Interrupted `sending` records also become uncertain. |
| P1 | The original unused `sendText` injected multiline text as keystrokes without checking the process. Activating it could send feedback to a shell. | Bracketed paste as one buffer; foreground agent, dead pane, copy mode and disabled-input checks; control-character rejection. Managed launches execute CLIs directly, and manual examples use `exec`. |
| P2 | Roles/status depended on pane position/count, and an extra active window could change the result. | Roles use stored pane IDs verified across the whole session. Swaps/new windows do not change recipients, and replacement panes do not silently inherit roles. |
| P2 | Exact window targets needed the `:` separator. On macOS, `ps comm` truncated paths; pi also changes its process title. | Targets match the tmux command's target type. Detection uses `ucomm`, foreground state, pi's process title and supported Node/symlink launch forms. |
| P2 | No test exercised the automatic workflow, bridge health or concurrent starts. | Health records, startup/queue locks, bounded batches, command timeouts, `status`, `messages`, `resolve`, unit tests, integration tests and native smoke tests. |

Main changes: [main.go](main.go), [bridge.go](bridge.go), [queue.go](queue.go), [tmux.go](tmux.go). English and Ukrainian [documentation](README.md) is synchronized.

## Validation

- `go test -race ./...` and `go vet ./...` passed.
- `TWOMUX_INTEGRATION=1 TWOMUX_NATIVE_SMOKE=1 go test -race -v ./...` passed. Fixtures exercised eight deliveries: corrections/rereview/approval, UTF-8, pane swaps, extra windows, concurrent bridge recovery, replay prevention, copy mode, disabled input, shell replacement, missing roles and foreign-session protection.
- A regression test proved explicit-queue sending with empty `TMUX`/`TMUX_PANE` and a `PATH` without tmux.
- Native smoke identified installed Codex and pi without submitting a model prompt.
- The live Codex/pi test passed: `hello.txt` contained exactly `2mux automatic review works` plus one LF, 28 bytes. Codex automatically requested review; pi independently checked the bytes and returned `APPROVED`; Codex received it and confirmed completion. Receipts and exact output are saved in [live-review-20260930.json](validation/live-review-20260930.json). Seven separately authored messages had distinct IDs and were each delivered once; repeated requests authored by an agent are not replay of one queue record.
- Built the local macOS arm64 `2mux` binary and cross-built Linux amd64. Test environment: Go 1.27.1, tmux 3.6a, Codex 0.159.2, pi 0.99.1.
- Removed temporary test sessions, the test project and runtime directories. Existing user tmux sessions were not used.

## Limits

`delivered` confirms submission to a terminal. Response quality, command execution and business correctness require their own checks. The live test proves one concrete scenario, not all CLI versions or configurations.

Finish initial trust/permission dialogs before exchanging task messages. Typing manually during delivery can mix prompt text. Process detection does not establish semantic readiness of every CLI dialog. Stopping tmux ends pane processes; independent CLI services manage their own lifecycle.

Records contain exchanged text in a private temporary directory. They remain after `stop` for inspection and are not a permanent archive. Linux was checked by the compiler, not live execution. Minimum Go 1.22 compatibility was not separately checked with that toolchain.

## Start

```sh
./2mux start --agents
```

Enter your task in Codex after the initial dialogs. Generated role instructions contain each agent's exact sending command. Use `./2mux status` and `./2mux messages` for diagnosis.

## Follow-up code review and improvements

The follow-up review reproduced four additional problems before applying fixes:

| Priority | Reproduced problem | Implemented improvement |
| --- | --- | --- |
| P2 | Six simultaneous first starts produced five `duplicate session` errors. Runtime startup locks did not cover session creation. | A private per-project lifecycle lock serializes `start` and `stop` before discovery and initialization, and releases before interactive attachment. Integration now covers concurrent first shell and agent starts. |
| P2 | `send --queue ""` silently fell back to current-project discovery; empty `--from` also bypassed explicit-value validation. | Reject empty send-option values before tmux discovery or queue access. |
| P2 | Queue records with an unknown/control-character sender or missing creation time reached the delivery callback. | Validate senders when enqueuing and validate sender/time metadata when reading records, protecting the message header and ordering. |
| P1 | An unrelated Node program was recognized as pi when a pi script path appeared in later arguments or a runtime option. | Recognize the actual supported script entry point or pi process title instead of scanning arbitrary arguments. Direct Node/Bun launches, pi symlinks and `bun run` remain covered. |

Validation after the fixes: fresh build, `go vet ./...` and `TWOMUX_INTEGRATION=1 TWOMUX_NATIVE_SMOKE=1 go test -race -count=1 -v ./...` passed. New regressions failed against the original implementation, then passed with the fixes. Integration exercised six concurrent shell starts across two different caller `TMPDIR`s, stop waiting for paused initialization, six concurrent agent starts and the existing eight-delivery review cycle. Lifecycle locking uses the fixed `/tmp/2mux-control-UID` namespace. Native smoke recognized the installed agents without a model prompt. Tests use unique projects and remove only their own control lock files after commands finish; tmux sockets and message records are isolated in test temporary directories. CLI and architecture documentation are synchronized in English and Ukrainian.

These changes do not establish model-level delivery guarantees or validate account/model configuration. The earlier saved live receipts remain historical evidence; this follow-up did not run a new model task.
