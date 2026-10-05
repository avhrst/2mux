# 2mux documentation

[Українська версія](README.uk.md) · [Project README](../README.md)

2mux opens a Codex worker and a Claude Code reviewer in a shared project, and delivers their feedback through a local queue and tmux bridge.

| Document | What you will find | Українською |
| --- | --- | --- |
| [User guide](guide.md) | Build, install, launch, review a task, reconnect and diagnose delivery problems. | [Посібник користувача](guide.uk.md) |
| [CLI reference](cli.md) | Commands, flags, sender selection, message limits, receipts and recovery actions. | [Довідник CLI](cli.uk.md) |
| [Architecture and development](architecture.md) | Source map, session ownership, queue states, process locks and validation. | [Архітектура та розробка](architecture.uk.md) |

For the recorded 30 September 2026 review and its validation limits, see [REVIEW.md](../REVIEW.md). The historical live test has [saved receipts](../validation/live-review-20260930.json).

Examples use `2mux` installed in `PATH`. Run project commands from the directory the agents should work on. Paths and message IDs shown as placeholders must be replaced with values from your own session.
