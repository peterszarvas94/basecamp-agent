# Basecamp webhook agent

Run Codex or Claude from trusted Basecamp mentions and assignments. Code jobs
use isolated Git worktrees, push a feature branch, and open a GitHub pull
request. Cards can move through `In progress`, `PR open`, `Done`, and failure
columns automatically.

## Install

Prerequisites: Go, Git, the
[Basecamp CLI](https://github.com/37signals/basecamp-cli), GitHub CLI, and the
Codex or Claude CLI.

```sh
curl -fsSL https://raw.githubusercontent.com/peterszarvas94/basecamp-webhook-agent/main/install.sh | sh
```

The installer starts an interactive setup that asks which bots, Basecamp
projects, and local repositories to use. It can also configure webhooks and a
systemd user service.

Or install the CLI directly with Go:

```sh
go install github.com/peterszarvas94/basecamp-webhook-agent/cmd/basecamp-webhook-agent@latest
```

The binary is written to `$(go env GOPATH)/bin` unless `GOBIN` is set.

To rerun setup or verify an installation:

```sh
basecamp-webhook-agent setup
basecamp-webhook-agent doctor
```

## Common commands

```sh
# Run directly
basecamp-webhook-agent serve

# Add another repository or project
basecamp-webhook-agent github repo add ~/Projects/my-app
basecamp-webhook-agent basecamp project add \
  --account 123 --project 456 --creator 789 --default-repo my-app

# Reconcile webhooks after changing configuration
basecamp-webhook-agent basecamp webhook sync
basecamp-webhook-agent github webhook sync

# Manage the background service
basecamp-webhook-agent service status
basecamp-webhook-agent service restart
```

Run `basecamp-webhook-agent --help` to see all commands. Every setup operation
is independently runnable and safe to repeat. Commands support `--config`,
`--dry-run`, `--json`, and `--non-interactive` where applicable.

Configuration and credentials stay outside this repository. Keep the account,
project, creator, and repository allowlists narrow: configured workers can use
your local GitHub credentials and currently run with sandbox bypass flags.

For architecture, development workflows, tests, and operational details, see
[AGENTS.md](AGENTS.md).

Released under the [MIT License](LICENSE). Vendored dependency notices are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
