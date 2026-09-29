# Basecamp agent

Run Codex or Claude from trusted Basecamp mentions and assignments. Code jobs
use isolated Git worktrees, push a feature branch, and open a GitHub pull
request. Cards can move through `In progress`, `PR open`, `Done`, and failure
columns automatically.

## Install

The only bootstrap prerequisite is Go. After installing this CLI, it can install
and check the external CLIs it uses:

```sh
curl -fsSL https://raw.githubusercontent.com/peterszarvas94/basecamp-agent/master/install.sh | sh
```

The installer starts an interactive setup that asks which bots, Basecamp
projects, and local repositories to use. It also installs a systemd user service
and gives the agent a public webhook URL (see below).

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

Railway operations deliberately delegate to the official Railway CLI rather
than requiring an MCP connection:

```sh
basecamp-agent install railway
basecamp-agent railway login
basecamp-agent railway deploy . --new --name my-app
basecamp-agent railway status
```

## Common commands

```sh
# Run directly
basecamp-agent serve

# Add another repository or project
basecamp-agent github repo add ~/Projects/my-app
basecamp-agent basecamp project add --account 123 --project 456 --creator 789 --default-repo my-app

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
