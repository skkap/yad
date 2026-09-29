#!/usr/bin/env bash
# Runs as the runner's user inside a work machine, from `yad-machine up`, after
# root.sh. Installs the harnesses into the user's own home, lays the spec's home
# files over it, and hands yad's configuration and the runner service to
# runner.sh.
set -euo pipefail

stage=$1
# shellcheck source=/dev/null
source "$stage/spec/machine.env"
cd "$HOME"
mkdir -p "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"

say() { printf -- '--> %s\n' "$*"; }

# Installers are downloaded, then run, never piped: a pipe into sh runs an empty
# stream when the download fails and reports success.
run_installer() {
	local url=$1 tmp
	shift
	tmp=$(mktemp)
	curl -fsSL -o "$tmp" "$url"
	"$@" "$tmp"
	rm -f "$tmp"
}

# Global npm packages land in the user's own prefix: the user has no sudo, and
# a harness it installed is one it may upgrade.
npm config set prefix "$HOME/.local" >/dev/null

for h in ${MACHINE_HARNESSES:-claude codex}; do
	case $h in
	claude)
		command -v claude >/dev/null 2>&1 || { say "Claude Code"; run_installer https://claude.ai/install.sh bash; } ;;
	codex)
		# Pinned: yad drives Codex through its app-server protocol and knows
		# the one version it was recorded against (pinned in
		# internal/adapter/codex/schema.go — a test there keeps this default
		# on it). Any other Codex is ready but warned about.
		want=${CODEX_VERSION:-0.157.1}
		have=$(codex --version 2>/dev/null | awk '{ print $NF }')
		[[ $have == "$want" ]] || { say "Codex $want"; npm install -g --silent "@openai/codex@$want"; } ;;
	*)
		echo "machine.env names harness $h, which yad-machine does not install — add it to the spec's provision.sh" >&2 ;;
	esac
done

command -v uv >/dev/null 2>&1 || { say "uv"; UV_NO_MODIFY_PATH=1 run_installer https://astral.sh/uv/install.sh sh; }

if [[ -d $stage/spec/home ]]; then
	say "home files"
	# Laid over the home, never mirrored: a harness login, gh's token and yad's
	# own state live here too, and a spec knows nothing of them.
	rsync -rlt "$stage/spec/home/" "$HOME/"
fi

bash "$stage/guest/runner.sh" "$stage"

echo
yad doctor
