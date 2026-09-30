# Basecamp agent

Run Codex or Claude from trusted Basecamp mentions and assignments. Code jobs
use isolated Git worktrees, push a feature branch, and open a GitHub pull
request. Cards can move through `In progress`, `PR open`, `Done`, and failure
columns automatically. A lightweight intent check routes ordinary questions
and Basecamp operations to assistant mode instead, without creating a worktree,
opening a pull request, or moving the triggering card through the code workflow.
When implementation starts from a todo, merging its pull request completes that
todo.

## Install

The only bootstrap prerequisite is Go. After installing this CLI, it can install
and check the external CLIs it uses:

```sh
curl -fsSL https://raw.githubusercontent.com/peterszarvas94/basecamp-agent/master/install.sh | sh
```

The installer starts an interactive setup that asks which bots, Basecamp
projects, and local repositories to use. It also installs a systemd user service,
registers webhooks for the public URL (see below), and creates any card columns
the agent moves cards through.

Or install the CLI directly with Go:

```sh
go install github.com/peterszarvas94/basecamp-agent/cmd/basecamp-agent@latest
basecamp-agent install git gh basecamp codex railway tailscale
basecamp-agent dependencies check
```

The binary is written to `$(go env GOPATH)/bin` unless `GOBIN` is set.

To rerun setup or verify an installation:

```sh
basecamp-agent setup
basecamp-agent doctor
```

Every setup capability is also available as a focused command. This makes a
fresh repository reproducible without using the wizard:

```sh
cd ~/Projects/my-app

# Existing GitHub repository:
basecamp-agent github repo add . --name my-app

# Or create the GitHub repository first:
basecamp-agent github repo create . --name my-app --visibility private

basecamp-agent --non-interactive setup \
  --repo . \
  --repo-name my-app \
  --public-url https://agent.example.com \
  --sync-webhooks \
  --account 123 \
  --project 456 \
  --creator 789 \
  --codex-person-id 101 \
  --install-service
```

## Public webhook URL

Basecamp and GitHub deliver webhooks to a public HTTPS URL, so the agent needs
one that forwards to its listen address (`127.0.0.1:8789` by default). The URL
must stay the same across restarts, because it is registered with Basecamp and
GitHub. Once it forwards to this machine, store it:

```sh
basecamp-agent endpoint set https://agent.example.com
basecamp-agent endpoint check
```

`endpoint set` removes webhooks registered for the previous URL and syncs
Basecamp and GitHub webhooks to the new one. `endpoint remove` removes them and
clears the URL.

If you do not have a domain, a tunnel can provide a permanent URL. Some options:

- [Tailscale Funnel](https://tailscale.com/kb/1223/funnel): free with a
  Tailscale account. `tailscale funnel --bg 8789` serves the agent at
  `https://<machine>.<tailnet>.ts.net` and survives reboots.
- [ngrok](https://ngrok.com): the
  free plan includes one static domain. Run
  `ngrok http --url=<your-static-domain> 8789` as a service.
- [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/):
  a named tunnel gives a stable hostname on a domain you manage in Cloudflare.
  Quick tunnels change their URL on every start, so they do not fit.
- [OpenTunnel](https://github.com/anomalyco/opentunnel): no account, but it
  is early beta software.
- A reverse proxy such as Caddy or nginx on a server and domain you control.

## Ops dashboard

The job dashboard is served at `<public-url>/ops`. Protect it with a token:

```sh
basecamp-agent config secret generate ops-token
basecamp-agent service restart
```

Print ready-to-open links, one for all jobs and one per Basecamp project:

```sh
basecamp-agent ops url
basecamp-agent ops url --project 456
```

Each project has its own page at `<public-url>/ops/projects/<project-id>`.
Opening a link with `?token=` stores a session cookie and redirects to the same
page without the token, so it does not stay in your browser history or links. Scripts can send `Authorization: Bearer <token>` instead. Rotate the
token with `--force` to end all sessions.

Railway support is deliberately limited to safe preview environments for agent
pull requests. It does not deploy or configure production. Point a configured
repository at an existing Railway project and service; the preview base defaults
to a persistent environment named `staging`, or choose another non-production
name with `--base`:

```sh
basecamp-agent install railway
basecamp-agent railway login
basecamp-agent railway preview prepare . --project <project-id> --service <service>
basecamp-agent railway preview setup . --confirm-safe-secrets
basecamp-agent railway preview prepare . --project <project-id> --service <service> --base agent-staging
basecamp-agent railway preview audit .
basecamp-agent railway preview status .
```

`preview prepare` first disables PR environments, then creates the persistent
base by copying service topology from production without deploying it. Replace
all inherited production credentials with staging or sandbox values before
running `preview setup --confirm-safe-secrets`; setup then connects the GitHub
source, selects that environment as the PR base, and enables bot PR previews.
Both commands refuse `production` as the preview base.

## Common commands

```sh
# Show everything configured
basecamp-agent list

# Run directly
basecamp-agent serve

# Add another repository or project
basecamp-agent github repo add ~/Projects/my-app
basecamp-agent project add --account 123 --project 456 --creator 789 --default-repo my-app
basecamp-agent project list
basecamp-agent project columns 456   # create missing card columns

# Reconcile webhooks after changing configuration
basecamp-agent basecamp webhook sync
basecamp-agent github webhook sync

# Manage the background service
basecamp-agent service status
basecamp-agent service restart
```

Run `basecamp-agent --help` to see all commands. Every setup operation
is independently runnable and safe to repeat. Commands support `--config`,
`--dry-run`, `--json`, and `--non-interactive` where applicable.

Configuration and credentials stay outside this repository. Keep the account,
project, creator, and repository allowlists narrow: configured workers can use
your local GitHub credentials and currently run with sandbox bypass flags.

For architecture, development workflows, tests, and operational details, see
[AGENTS.md](AGENTS.md).

Released under the [MIT License](LICENSE). Vendored dependency notices are in
the [`licenses`](licenses) directory.
