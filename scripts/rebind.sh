#!/usr/bin/env bash
# Rebind every active plugin Key to all current OAuth accounts (idempotent).
#
# Convenience wrapper around bind-all-keys-to-oauth.sh. Reads the host management
# secret from $CPA_MANAGEMENT_SECRET, or from a sibling .cpa_secret file (git-ignored;
# create it on the server with: printf %s '<secret>' > .cpa_secret && chmod 600 .cpa_secret).
#
# All flags pass through, e.g.: ./rebind.sh --dry-run | --replace | --include-disabled-keys
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${CPA_MANAGEMENT_SECRET:-}" ] && [ -f "$DIR/.cpa_secret" ]; then
  CPA_MANAGEMENT_SECRET="$(tr -d '[:space:]' < "$DIR/.cpa_secret")"; export CPA_MANAGEMENT_SECRET
fi
exec bash "$DIR/bind-all-keys-to-oauth.sh" "$@"
