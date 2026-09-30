# Agent guide

This document contains implementation and operational context for agents
working on this repository. Keep the README short and user-oriented; place
developer-facing detail here.

## Purpose and architecture

The service accepts trusted Basecamp work and dispatches it to local Codex or
Claude CLIs. Its main pieces are:

- Every job first selects exactly one allowlisted repository (see below); a
  job with no match fails. A tool-free intent pass then runs in that
  repository's checkout and routes the request to assistant or implementation
  mode. Assistant mode runs in the same checkout, may read it for context, and
  may perform project-scoped Basecamp work, but never edits, commits, or pushes
  and stays out of the automatic card lifecycle. Implementation mode uses the
  full worktree, PR, and card workflow. Intent models are deliberately
  economical; making mode-specific models configurable is future work.

- `main.go`: configuration, Basecamp webhook verification, dispatch, worker
  execution, and HTTP server startup.
- `prompts/*.md`: embedded model prompt templates. Keep model-facing prose in
  these Markdown files rather than Go string literals; `prompts.go` supplies
  typed template data and renders them.
- `cli.go`: Cobra commands, setup, config mutation, webhook reconciliation,
  diagnostics, and systemd management.
- `chat.go`: Campfire event-feed polling and replay.
- `worktree.go`: repository selection, worktrees, branch publishing, and PRs.
- `github.go`: signed GitHub PR webhooks and Basecamp card transitions.
- `ops.go`: job persistence, recovery, retry/stop actions, and operator UI
  handlers.
- `ops.templ`: the dashboard's templ components. All dashboard markup lives
  here; Go code only builds view data and never builds HTML with strings.
  Style with DaisyUI components and theme colors, and put behavior in Datastar
  attributes and signals (the job output is the `output` signal). Do not add
  custom CSS or JS files: `static/` holds only `datastar.js`. If a script is
  ever unavoidable, make it an ES module with an import map, never an IIFE.
  Regenerate `ops_templ.go` with `go tool templ generate` and commit it, so
  `go install` works without templ; never edit it by hand.
- `endpoint.go`: the public URL, its health check, and moving Basecamp and
  GitHub webhooks when it changes.

Public HTTP routes are `/` (liveness), `/webhook`, `/github/webhook`, and the
optional `/ops` dashboard.

## CLI design

`setup` orchestrates independently useful operations. Never put an essential
setup capability only inside the wizard. Expose new behavior through a focused
command under `config`, `agent`, `project`, `endpoint`, `ops`, `basecamp`,
`github`, or `service`. Everything configured must be listable: each area has
a `list` or `show`, and `basecamp-agent list` shows it all.

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

Railway support is preview-only. It may connect an allowlisted repository to an
existing Railway project and service and configure a persistent non-production
environment as the base for agent-created PR environments. It must never deploy,
redeploy, promote, roll back, expose domains for, or mutate production. The
preview base defaults to `staging`, is stored as
`preview_base_environment`, and must reject `production`.

## Configuration and repositories

Live config defaults to `~/.config/basecamp-agent/config.json`; state
defaults to `~/.local/state/basecamp-agent/state.json`.
`config.example.json` is the checked-in reference.

`allowed_repos` entries contain `name`, `path`, and optional `aliases`.
String-only entries are rejected. A job selects exactly one repo by matching
its name, path basename, or alias in the Basecamp item or parent title.
`project_repos` supplies a default. Ambiguous or unmatched jobs must fail instead
of guessing.

Work runs in `worktree_root/<repo>/<agent>/bc-<event-id>` from `origin/HEAD`.
The dispatcher verifies the branch and clean status, pushes only the feature
branch, opens a PR, and never merges. Clean worktrees without commits are
removed. Dirty, failed, and PR-bearing worktrees remain for recovery or review.

## Public URL

`public_url` is installation-global and never belongs to a repository or
Basecamp project. The CLI does not create or manage tunnels: users supply any
stable HTTPS URL that forwards to `listen`, and the README suggests tunnel
options. `endpoint set` removes webhooks for the previous URL before syncing
the new one. Never bake a specific host into code, examples, or the installer.

## Basecamp trust boundaries

Only configured account, project, and creator IDs are accepted. Treat webhooks
as notifications, not authority: verify events through the Basecamp event feed
and refetch recordings. Require real person-ID mention markup or newly added
assignee IDs; plain text such as `@Claude` is insufficient.

Campfire is polled because it has no webhook type. One event-feed query covers
every allowed project and creator, using the Codex bot profile (or the first
configured one). It reads at most two pages and 4 MiB per poll and never replays
old history: the saved feed position is tied to the project and creator
allowlists, so changing them, or a position Basecamp rejects, restarts polling
from now.

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

For implementation jobs originating from a todo, merging the associated pull
request completes the todo. Pull-request open/reopen events do not change todos,
and closing without merging leaves them open.

Column lookup is case-insensitive. A failed column lookup or move is logged and
skipped; it must not fail the coding job.

## Operations and security

The `/ops` dashboard is served on the same public URL as the webhooks, so
require `ops.token` (`config secret generate ops-token`) whenever the URL is
public. The token is accepted as a bearer header, or once as `?token=`, which
is exchanged for an HttpOnly session cookie (an HMAC of the token, so rotating
the token ends every session) and redirected to the URL without it. Never
put the token back into rendered links. `/ops/projects/<id>` shows one Basecamp
project's jobs; a job's project comes from the `/buckets/<id>/` part of its
target, and project pages pass `?project=` to their stream and actions.
`basecamp-agent ops url` prints token-bearing links, so treat its output as a
secret. Persisted job files are durable; SSE
is only a live notification channel.

On restart, reconcile running jobs. If a PR opened before reporting completed,
recover that outcome instead of calling the job failed. Failed jobs get the
same check at startup: if the newest run's own commit is the pushed branch head
and the branch has a PR, mark it completed and move the card to PR open.
Retries inspect prior status, logs, and worktrees, then use a fresh worktree.

Parse external CLI output from stdout only, and tolerantly: shims such as mise
print extra lines around the answer.

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
go tool templ generate -path cmd/basecamp-agent
go tool templ fmt cmd/basecamp-agent
gofmt -w cmd/basecamp-agent/*.go
go test -race ./...
go vet ./...
go build ./cmd/basecamp-agent
sh -n install.sh
git diff --check
```

Before pushing any repository change:

1. Install the current worktree build into the local binary path:
   `GOBIN="$HOME/.local/bin" go install ./cmd/basecamp-agent`.
2. Use that freshly installed binary for any setup or configuration operations
   required by the change. At minimum, validate the existing external config
   with `basecamp-agent config validate`.
3. If the user service is active and runtime behavior changed, restart it and
   verify that it remains active.
4. Push only after installation and local validation succeed.

Never replace the external live config with example data, and never stage it.

In restricted environments, set `GOCACHE` to a writable directory such as
`/tmp/basecamp-agent-gocache`.

Optional live tests:

```sh
BASECAMP_CHAT_TEST_LINE=<line-id> go test ./cmd/basecamp-agent
BASECAMP_WORKTREE_TEST=1 go test -run TestPrepareWorktreeLive ./cmd/basecamp-agent
BASECAMP_ASSIGNMENT_TEST=1 go test -run TestAssignmentActorMayDifferFromTodoCreator ./cmd/basecamp-agent
```

Replay commands accept verified events from the last two hours:

```sh
basecamp-agent serve --replay-chat-event <event-id>
basecamp-agent serve --replay-todo-assignment <event-id>
```
