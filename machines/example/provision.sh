#!/usr/bin/env bash
# Optional. Runs as root inside the machine after the kit's own provisioning,
# on every `up`, so it must be idempotent. AGENT_USER names the runner's user.
# Install here what this machine's work needs beyond the kit's base tools.
set -euo pipefail

apt-get install -y -q --no-install-recommends postgresql-client
