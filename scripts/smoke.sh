#!/usr/bin/env bash
# One real run through yad hub, by the commands an operator types: hub serve
# on loopback, a registration token, connect, the runner in the foreground,
# submit, watch. The same path as the end-to-end tests in cmd/yad/e2e_test.go,
# with the real harness in place of the fake.
#
#   scripts/smoke.sh claude    make smoke
#   scripts/smoke.sh codex     make smoke-codex
#
# It spends a few cents of the logged-in account (the cheapest model, a
# one-line answer and one file read), so it is run by hand — never in CI,
# never from `make check`. SMOKE_MODEL overrides the model. Everything it
# writes lives in a throwaway directory that is removed on exit; no token is
# printed.
set -euo pipefail

yad=${YAD_BIN:-bin/yad}
harness=${1:-claude}
# Per harness: the cheapest sensible model, how the run is asked to read the
# file, and how `yad hub watch` shows that read. gpt-5.6-luna is the model the
# Codex fixtures were recorded on, the cheapest the account offers.
case $harness in
claude)
	model=${SMOKE_MODEL:-haiku}
	ask="Use the Read tool to read %s, then reply with its contents only, on one line."
	tool='^→ Read'
	command -v claude >/dev/null || { echo "smoke: claude is not on PATH — install Claude Code and log in" >&2; exit 2; }
	;;
codex)
	model=${SMOKE_MODEL:-gpt-5.6-luna}
	ask="Run the shell command \`cat %s\` and reply with its output only, on one line."
	tool='^→ shell'
	command -v codex >/dev/null || { echo "smoke: codex is not on PATH — install Codex and log in" >&2; exit 2; }
	# A logged-out codex fails the run only after the runner has started it;
	# said here, the cause is plain.
	codex login status >/dev/null 2>&1 || { echo "smoke: codex is not logged in — run \`codex login\`" >&2; exit 2; }
	;;
*)
	echo "smoke: no smoke for harness \"$harness\" — use claude or codex" >&2
	exit 2
	;;
esac
[[ -x $yad ]] || { echo "smoke: no $yad — run make build first" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/yad-smoke.XXXXXX")
pids=()
cleanup() {
	for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

# A profile of its own: nothing touches the operator's config, data or hub.
export YAD_CONFIG_DIR=$work/config YAD_DATA_DIR=$work/data
# Private, as yad makes them itself: the daemon refuses a data directory other
# users can reach, since its control socket lives there.
mkdir -m 700 "$YAD_CONFIG_DIR" "$YAD_DATA_DIR"
mkdir -p "$work/files"
# The file the run reads carries a fresh nonce, so an answer that contains
# it came from this run's read of it and nowhere else.
nonce=smoke-$RANDOM$RANDOM
printf '%s\n' "$nonce" >"$work/files/note.txt"

"$yad" hub serve --listen 127.0.0.1:0 >"$work/hub.log" 2>&1 &
pids+=($!)
hub=
for _ in $(seq 100); do
	hub=$(sed -n 's|^yad hub serving protocol v1 at \(http://[^ ]*\)/v1 .*|\1|p' "$work/hub.log")
	[[ -n $hub ]] && break
	sleep 0.1
done
[[ -n $hub ]] || { echo "smoke: yad hub did not start:" >&2; cat "$work/hub.log" >&2; exit 1; }
echo "smoke: yad hub at $hub"

"$yad" hub admin-token create >/dev/null
# The registration token goes from one command to the other through a pipe,
# never through argv or the terminal (decision 0020).
# Its notice goes to a file rather than the terminal; on a failure, the cause
# is shown from there.
"$yad" hub token create 2>"$work/token.err" | "$yad" connect "$hub/v1" --token - --name smoke ||
	{ echo "smoke: registering the runner failed:" >&2; cat "$work/token.err" >&2; exit 1; }

"$yad" daemon start --foreground >"$work/runner.log" 2>&1 &
daemon=$!
pids+=("$daemon")

# shellcheck disable=SC2059 # the format is ours, chosen above
run=$("$yad" hub submit --hub "$hub" --harness "$harness" --model "$model" "$(printf "$ask" "$work/files/note.txt")")
echo "smoke: submitted $run"

# macOS has no timeout(1): the watch runs in the background under a deadline,
# and its output is followed live from the file it writes.
"$yad" hub watch --hub "$hub" "$run" >"$work/watch.log" 2>&1 &
watch=$!
pids+=("$watch")
tail -f "$work/watch.log" &
pids+=($!)
deadline=$((SECONDS + ${SMOKE_TIMEOUT:-180}))
while kill -0 "$watch" 2>/dev/null; do
	if ((SECONDS > deadline)); then
		echo "smoke: FAILED — no result within ${SMOKE_TIMEOUT:-180}s" >&2
		kill "$watch"
	fi
	# A runner that exited will never claim the run; waiting out the deadline
	# would only hide why.
	if ! kill -0 "$daemon" 2>/dev/null; then
		echo "smoke: FAILED — the runner exited; it said:" >&2
		cat "$work/runner.log" >&2
		kill "$watch"
		exit 1
	fi
	sleep 1
done
status=0
wait "$watch" || status=$?
sleep 0.2 # the last lines reach the terminal before the verdict

if [[ $status -ne 0 ]]; then
	echo "smoke: FAILED — the run did not succeed; the runner said:" >&2
	cat "$work/runner.log" >&2
	exit 1
fi
# A whole line: the tool's output shows the nonce too, and only the answer
# has it alone.
if ! grep -qx -- "$nonce" "$work/watch.log"; then
	echo "smoke: FAILED — the run succeeded but its answer lacks the file's contents ($nonce)" >&2
	exit 1
fi
if ! grep -q "$tool" "$work/watch.log"; then
	echo "smoke: FAILED — the answer is right but no tool call reading the file was streamed" >&2
	exit 1
fi
echo "smoke: ok — one real $harness run ($model), end to end, through yad hub"
