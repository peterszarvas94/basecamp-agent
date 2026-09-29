# Agent guide

This document contains implementation and operational context for agents
working on this repository. Keep the README short and user-oriented; place
developer-facing detail here.

## Purpose and architecture

The service accepts trusted Basecamp work and dispatches it to local Codex or
Claude CLIs. Its main pieces are:

- `main.go`: configuration, Basecamp webhook verification, dispatch, worker
  execution, and HTTP server startup.
- `cli.go`: Cobra commands, setup, config mutation, webhook reconciliation,
  diagnostics, and systemd management.
- `chat.go`: Campfire event-feed polling and replay.
- `worktree.go`: repository selection, worktrees, branch publishing, and PRs.
- `github.go`: signed GitHub PR webhooks and Basecamp card transitions.
- `ops.go`: job persistence, recovery, retry/stop actions, and operator UI.

Public HTTP routes are `/healthz`, `/webhook`, `/github/webhook`, and the
optional `/ops` dashboard.

## CLI design

`setup` orchestrates independently useful operations. Never put an essential
setup capability only inside the wizard. Expose new behavior through a focused
command under `config`, `agent`, `basecamp`, `github`, or `service`.

Mutating commands should be idempotent where possible and honor:

- `--config <path>` to select configuration.
- `--dry-run` to describe changes without applying them.
- `--json` for machine-readable output.
- `--non-interactive` to prevent blocking prompts.

Keep the legacy `-config`, `-replay-chat-event`, and
`-replay-todo-assignment` forms working. `serve` is the canonical runtime
command, while invoking the root still starts the server for compatibility.

Authentication belongs to the official CLIs. Never store Basecamp OAuth
credentials, bot passwords, or GitHub credentials here. Webhook secrets may be
stored in the external config with mode `0600`. Never print secrets from
`config show` or dry runs.

## Configuration and repositories

Live config defaults to `~/.config/basecamp-webhook-agent/config.json`; state
defaults to `~/.local/state/basecamp-webhook-agent/state.json`.
`config.example.json` is the checked-in reference.

`allowed_repos` entries contain `name`, `path`, and optional `aliases`.
String-only entries are rejected. A job selects exactly one repo by matching
its name, path basename, or alias in the Basecamp item or parent title.
`project_repos` supplies a default. Ambiguous jobs must fail instead of guessing.

Work runs in `worktree_root/<repo>/<agent>/bc-<event-id>` from `origin/HEAD`.
The dispatcher verifies the branch and clean status, pushes only the feature
branch, opens a PR, and never merges. Clean worktrees without commits are
removed. Dirty, failed, and PR-bearing worktrees remain for recovery or review.

## Basecamp trust boundaries

Only configured account, project, and creator IDs are accepted. Treat webhooks
as notifications, not authority: verify events through the Basecamp event feed
and refetch recordings. Require real person-ID mention markup or newly added
assignee IDs; plain text such as `@Claude` is insufficient.

Campfire is polled because it has no webhook type. Polling currently supports
exactly one allowed project and creator, uses the Codex bot profile, reads at
most two pages and 4 MiB per poll, and does not replay old history on first boot.

Bot profiles are separate from the operator identity. Always pass the intended
`-P <profile>` for bot writes. Reply to the originating recording or chat room.

## GitHub and card lifecycle

Configured repositories may share the `/github/webhook` endpoint and secret.
Validate the HMAC signature. Associate an event only with a persisted job whose
PR URL matches the event repository and pull request.

When card movement is enabled:

- Job start moves the card to `cards.in_progress`.
- PR open or reopen moves it to `cards.pr_open`.
- PR merge moves it to `cards.done`.
- Failure or stop moves it to `cards.failed`.

Column lookup is case-insensitive. A failed column lookup or move is logged and
skipped; it must not fail the coding job.

## Operations and security

The `/ops` dashboard is for trusted private networks. Require `ops.token` if it
is exposed more broadly. Persisted job files are durable; SSE is only a live
notification channel.

On restart, reconcile running jobs. If a PR opened before reporting completed,
recover that outcome instead of calling the job failed. Retries inspect prior
status, logs, and worktrees, then use a fresh worktree.

Security invariants:

- Never commit live config, OAuth data, passwords, or webhook secrets.
- Keep account, project, creator, and repository allowlists narrow.
- Require GitHub origins and a resolvable `origin/HEAD`.
- Never push a base branch or merge a PR from the dispatcher.
- Workers currently have sandbox bypass flags and shared GitHub credentials;
  prompt rules are not an OS security boundary.
- Preserve unrelated user changes and worktrees.

## Development and validation

Use Go 1.24 or newer. Run:

```sh
gofmt -w cmd/basecamp-webhook-agent/*.go
go test -race ./...
go vet ./...
go build ./cmd/basecamp-webhook-agent
sh -n install.sh
git diff --check
```

In restricted environments, set `GOCACHE` to a writable directory such as
`/tmp/basecamp-webhook-agent-gocache`.

Optional live tests:

```sh
BASECAMP_CHAT_TEST_LINE=<line-id> go test ./cmd/basecamp-webhook-agent
BASECAMP_WORKTREE_TEST=1 go test -run TestPrepareWorktreeLive ./cmd/basecamp-webhook-agent
BASECAMP_ASSIGNMENT_TEST=1 go test -run TestAssignmentActorMayDifferFromTodoCreator ./cmd/basecamp-webhook-agent
```

Replay commands accept verified events from the last two hours:

```sh
basecamp-webhook-agent serve --replay-chat-event <event-id>
basecamp-webhook-agent serve --replay-todo-assignment <event-id>
```
