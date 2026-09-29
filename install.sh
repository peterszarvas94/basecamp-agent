#!/bin/sh
set -eu

module=github.com/peterszarvas94/basecamp-agent/cmd/basecamp-agent
version=${BASECAMP_AGENT_VERSION:-latest}
install_dir=${BASECAMP_AGENT_INSTALL_DIR:-"$HOME/.local/bin"}

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required to install basecamp-agent." >&2
  echo "Install Go, then rerun this command." >&2
  exit 1
fi

mkdir -p "$install_dir"
GOBIN="$install_dir" go install "$module@$version"

echo "Installed basecamp-agent to $install_dir/basecamp-agent"
case :$PATH: in
  *:"$install_dir":*) ;;
  *) echo "Add $install_dir to PATH before continuing." ;;
esac

if [ "${BASECAMP_AGENT_SKIP_SETUP:-0}" != 1 ] && [ -t 1 ] && [ -r /dev/tty ]; then
  "$install_dir/basecamp-agent" setup </dev/tty
else
  echo "Run: basecamp-agent setup"
  echo "Setup asks for a public HTTPS URL that forwards to the agent; the README lists tunnel options."
fi
