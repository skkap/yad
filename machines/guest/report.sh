#!/usr/bin/env bash
# yad-machine-report — what a work machine says about itself, as one JSON
# object, for `yad-machine status` and `login` on the host. Installed into
# /usr/local/bin by root.sh and run as the runner's user.
#
# Everything in it is yad's own answer — the service, `yad status`, the
# capability document a hub receives, `yad doctor`, which alone says a harness
# needs a login rather than being broken, `yad account list` — plus the spec's
# checks: each executable in ~/.config/yad-machine/checks/ is run, and its exit
# status and first line of output are its answer. A check says what is wrong
# and what to do about it in that line; it never prints a secret.
set -uo pipefail

json_or_null() {
	local out
	out=$("$@" 2>/dev/null) && jq -e . >/dev/null 2>&1 <<<"$out" && printf '%s' "$out" || printf 'null'
}

service=$(systemctl --user is-active yad-runner-default 2>/dev/null || true)

checks='[]'
dir=$HOME/.config/yad-machine/checks
if [[ -d $dir ]]; then
	for c in "$dir"/*; do
		[[ -f $c && -x $c ]] || continue
		# A check is the spec's code and may hang on the network; twenty
		# seconds is a check that has failed.
		line=$(timeout 20 "$c" 2>&1 | head -n 1)
		status=${PIPESTATUS[0]}
		checks=$(jq -c --arg n "$(basename "$c")" --arg d "$line" --argjson ok "$([[ $status == 0 ]] && echo true || echo false)" \
			'. + [{name: $n, ok: $ok, detail: $d}]' <<<"$checks")
	done
fi

jq -n \
	--arg service "${service:-unknown}" \
	--argjson status "$(json_or_null yad status --json)" \
	--argjson capabilities "$(json_or_null yad harnesses)" \
	--argjson doctor "$(json_or_null yad doctor --json)" \
	--argjson accounts "$(json_or_null yad account list --json)" \
	--argjson checks "$checks" \
	'{service: $service, status: $status, harnesses: ($capabilities.harnesses // []), doctor: $doctor, accounts: $accounts, checks: $checks}'
