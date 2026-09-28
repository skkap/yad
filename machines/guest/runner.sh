#!/usr/bin/env bash
# Runs as the runner's user inside a work machine, from agent.sh, with the
# stage directory as its argument. Brings yad's config.toml in line with the
# spec's and restarts the runner service only when something the runner reads
# at start has changed: a restart drains every run the runner holds, so an `up`
# that changed nothing must not make one.
#
# Separate from agent.sh so it can be run, and tested, without the network or
# a VM: it needs only yad and systemctl.
set -euo pipefail

stage=$1

say() { printf -- '--> %s\n' "$*"; }

# yad's own resolution for the default profile (internal/config/paths.go).
cfg=${YAD_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/yad}/config.toml
unit=yad-runner-default.service
# The restart this machine is owed, and why, one reason a line. Kept on disk
# rather than in a variable: an up that stops between changing config.toml and
# restarting the runner — a dropped connection, a failed install — leaves it
# here, and the next up makes the restart though it changes nothing itself.
owed=${XDG_STATE_HOME:-$HOME/.local/state}/yad-machine/restart-owed
mkdir -p "$(dirname "$owed")"

owe() {
    [[ -f $owed ]] && grep -qxF -- "$1" "$owed" && return 0
    printf '%s\n' "$1" >>"$owed"
}

# root.sh leaves this when it installed a yad different from the one there:
# the runner still running is the old binary.
if [[ -f $stage/yad-replaced ]]; then owe "yad was replaced"; fi

checksum() { if [[ -f $cfg ]]; then cksum <"$cfg"; fi; }

say "yad config.toml"
# Read whole, not piped into grep -q: under pipefail, grep leaving at its first
# match can end the pipeline on yad's SIGPIPE and read as "no such command".
if grep -q 'config apply' <<<"$(yad help 2>/dev/null)"; then
    # The spec owns every setting but the connections, and its accounts are
    # added to the machine's, never subtracted from them (decision 0058).
    # yad does the merge, under the lock its other writers take, so the
    # file stays as yad writes it and a hub adding an account meanwhile is
    # kept. The checksum, not yad's wording, says whether it wrote the file.
    before=$(checksum)
    yad config apply "$stage/spec/config.toml"
    if [[ $(checksum) != "$before" ]]; then owe "config.toml changed"; fi
elif [[ ! -f $cfg ]]; then
    # A yad from before `config apply` — a pinned YAD_VERSION, or the
    # latest release before it shipped: the spec is seeded once, as the kit
    # did then.
    install -d -m 0700 "$(dirname "$cfg")"
    install -m 0600 "$stage/spec/config.toml" "$cfg"
    owe "config.toml was written"
elif ! diff -q "$stage/spec/config.toml" "$cfg" >/dev/null; then
    echo "note: this yad has no \`yad config apply\`, so the spec's config.toml was not brought onto $cfg — a newer yad (YAD_VERSION, or --yad) does it. Compare with:"
    echo "  diff $stage/spec/config.toml $cfg"
fi

# Enabled and active, or it is installed again: a machine whose runner is not
# running is owed a start whatever else changed.
if ! systemctl --user is-enabled --quiet "$unit" 2>/dev/null ||
    ! systemctl --user is-active --quiet "$unit" 2>/dev/null; then
    owe "the runner was not running"
fi

if [[ -s $owed ]]; then
    say "runner service — $(awk 'NR > 1 { printf "; " } { printf "%s", $0 }' "$owed")"
    yad service install
    rm -f -- "$owed"
else
    say "runner service left running: nothing it reads at start changed"
fi
