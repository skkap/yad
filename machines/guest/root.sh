#!/usr/bin/env bash
# Runs as root inside a work machine, from `yad-machine up`, with the stage
# directory as its argument. Everything here is idempotent: `up` runs it again
# on every update.
set -euo pipefail

stage=$1
AGENT_USER=agent
# shellcheck source=/dev/null
source "$stage/spec/machine.env"
export DEBIAN_FRONTEND=noninteractive

say() { printf -- '--> %s\n' "$*"; }

say "packages"
apt-get update -q
# The tools a coding run reaches for without asking: git and gh for sources and
# pull requests, jq and ripgrep for reading, Python and a compiler for a
# repository's own setup. A spec's provision.sh adds what its work needs.
apt-get install -y -q --no-install-recommends \
	ca-certificates curl gnupg git jq ripgrep fd-find unzip zip xz-utils \
	less file tmux sqlite3 rsync python3 python3-venv python3-pip \
	build-essential nftables

if ! command -v gh >/dev/null 2>&1; then
	say "gh"
	# GitHub's own apt repository: Ubuntu's gh lags far enough behind that
	# `gh auth setup-git` and fine-grained tokens behave differently.
	install -d -m 0755 /etc/apt/keyrings
	curl -fsSL -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
		https://cli.github.com/packages/githubcli-archive-keyring.gpg
	chmod a+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
		>/etc/apt/sources.list.d/github-cli.list
	apt-get update -q
	apt-get install -y -q gh
fi

# Node from nodejs.org, checked against its SHASUMS256.txt. Ubuntu 24.04's is
# 18, past its end of life, and Codex and most JavaScript repositories want a
# current one. NODE_MAJOR in machine.env picks the line.
node_major=${NODE_MAJOR:-22}
if [[ $(node --version 2>/dev/null | sed 's/^v\([0-9]*\).*/\1/') != "$node_major" ]]; then
	say "node $node_major"
	case $(uname -m) in aarch64) arch=arm64 ;; x86_64) arch=x64 ;; *) arch=$(uname -m) ;; esac
	base=https://nodejs.org/dist/latest-v$node_major.x
	tmp=$(mktemp -d)
	curl -fsSL -o "$tmp/SHASUMS256.txt" "$base/SHASUMS256.txt"
	file=$(awk -v a="linux-$arch.tar.xz" '$2 ~ a"$" { print $2 }' "$tmp/SHASUMS256.txt")
	[[ -n $file ]] || { echo "no node $node_major build for linux-$arch at $base" >&2; exit 1; }
	curl -fsSL -o "$tmp/$file" "$base/$file"
	(cd "$tmp" && grep " $file\$" SHASUMS256.txt | sha256sum -c -)
	# The npm and corepack a Node release bundles are unpacked over the ones
	# already there, and files the new release no longer has are left behind:
	# Node 24 over 22 left an npm that failed on every command ("Class extends
	# value undefined"). Removing the bundled two first makes the upgrade clean;
	# packages installed globally beside them are left alone.
	rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack
	tar -C /usr/local --strip-components=1 --no-same-owner -xJf "$tmp/$file" \
		--exclude='*/CHANGELOG.md' --exclude='*/LICENSE' --exclude='*/README.md'
	rm -rf "$tmp"
fi

if ! id "$AGENT_USER" >/dev/null 2>&1; then
	say "user $AGENT_USER"
	useradd --create-home --shell /bin/bash "$AGENT_USER"
fi
# Lingering starts the user's systemd manager at boot, with nobody logged in,
# so the runner's user service runs whenever the machine does.
loginctl enable-linger "$AGENT_USER"
uid=$(id -u "$AGENT_USER")
for _ in $(seq 1 30); do [[ -d /run/user/$uid ]] && break; sleep 1; done

say "egress rules for $AGENT_USER"
# The runner's user reaches the internet and nothing closer. Lima's gateway is
# the host itself, and it forwards to the host's loopback — every service there
# that trusts localhost — so it is closed first, then the LAN, link-local and
# Tailscale's CGNAT range behind it. Loopback inside the machine stays open:
# DNS goes through systemd-resolved's stub, which runs as its own user and so is
# not held by these rules. MACHINE_EGRESS_ALLOW lists the private addresses a
# spec's work genuinely needs.
install -d -m 0755 /etc/yad-machine
allow=${MACHINE_EGRESS_ALLOW:-}
{
	echo "# Written by yad-machine (machines/guest/root.sh); edits are replaced on the next up."
	echo "table inet yad_machine {}"
	echo "delete table inet yad_machine"
	echo "table inet yad_machine {"
	echo "  chain output {"
	echo "    type filter hook output priority 0; policy accept;"
	echo "    meta skuid != \"$AGENT_USER\" accept"
	echo "    oif \"lo\" accept"
	if [[ -n $allow ]]; then
		echo "    ip daddr { ${allow// /, } } accept"
	fi
	echo "    ip daddr { 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16 } counter reject"
	echo "    ip6 daddr { fc00::/7, fe80::/10 } counter reject"
	echo "  }"
	echo "}"
} >/etc/yad-machine/egress.nft
nft -c -f /etc/yad-machine/egress.nft
# nftables.service loads /etc/nftables.conf at boot. It only includes this
# table, and never flushes the ruleset, so rules a spec's tools add (Docker's)
# survive a reload.
printf '#!/usr/sbin/nft -f\ninclude "/etc/yad-machine/egress.nft"\n' >/etc/nftables.conf
systemctl enable --quiet nftables
nft -f /etc/yad-machine/egress.nft

say "yad"
# Root-owned, so a run cannot replace the runner that runs it — nor the report
# the host reads to say whether the machine is well.
#
# Only when it differs: the runner is restarted for a new binary, and a restart
# drains the runs it holds. The mark tells runner.sh.
if ! cmp -s "$stage/yad" /usr/local/bin/yad; then
    install -m 0755 "$stage/yad" /usr/local/bin/yad
    touch "$stage/yad-replaced"
fi
install -m 0755 "$stage/guest/report.sh" /usr/local/bin/yad-machine-report

if [[ -f $stage/spec/provision.sh ]]; then
	say "the spec's provision.sh"
	STAGE=$stage AGENT_USER=$AGENT_USER bash "$stage/spec/provision.sh"
fi
