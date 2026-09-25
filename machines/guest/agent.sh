#!/usr/bin/env bash
# Runs as the runner's user inside a work machine, from `yad-machine up`, after
# root.sh. Installs the harnesses into the user's own home, lays the spec's home
# files over it, seeds yad's configuration and installs the runner service.
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
		# the versions it was recorded against (pinned in
		# internal/adapter/codex/schema.go — a test there keeps this default
		# one of them). A newer Codex is ready but warned about.
		want=${CODEX_VERSION:-0.147.0}
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

cfg=${XDG_CONFIG_HOME:-$HOME/.config}/yad/config.toml
if [[ ! -f $cfg ]]; then
	say "yad config.toml"
	install -d -m 0700 "$(dirname "$cfg")"
	install -m 0600 "$stage/spec/config.toml" "$cfg"
elif ! diff -q "$stage/spec/config.toml" "$cfg" >/dev/null; then
	# After the first up, yad owns the file: connect and account add write to
	# it. Replacing it would disconnect every hub.
	echo "note: $cfg differs from the spec's config.toml — expected once a hub is connected; the machine's copy is kept. Compare with:"
	echo "  diff $stage/spec/config.toml $cfg"
fi

say "runner service"
yad service install

echo
yad doctor
