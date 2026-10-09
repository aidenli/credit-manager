#!/usr/bin/env bash
# Bind every plugin Key to all current OAuth accounts via the credit-manager management API.
#
# Why the management API (not the SQLite DB directly): only the running host can
# enumerate OAuth accounts and report the exact account IDs the scheduler matches
# bindings against. Reading the auth files off disk cannot reproduce those IDs.
#
# Usage:
#   export CPA_MANAGEMENT_SECRET=<host management secret>
#   ./bind-all-keys-to-oauth.sh                      # merge OAuth set onto each key (idempotent)
#   ./bind-all-keys-to-oauth.sh --dry-run            # show what would change, write nothing
#   ./bind-all-keys-to-oauth.sh --replace            # overwrite each key with exactly the OAuth set
#   ./bind-all-keys-to-oauth.sh --include-disabled-keys
#   ./bind-all-keys-to-oauth.sh --base-url http://127.0.0.1:8317
set -euo pipefail

BASE_URL="http://127.0.0.1:8317"
SECRET="${CPA_MANAGEMENT_SECRET:-}"
REPLACE=0
DRY_RUN=0
ACTIVE_ONLY=1

while [ $# -gt 0 ]; do
  case "$1" in
    --base-url) BASE_URL="$2"; shift 2 ;;
    --secret) SECRET="$2"; shift 2 ;;
    --replace) REPLACE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --include-disabled-keys) ACTIVE_ONLY=0; shift ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

[ -n "$SECRET" ] || { echo "management secret required: --secret or \$CPA_MANAGEMENT_SECRET" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

BASE="${BASE_URL%/}/v0/management/credit-manager"
AUTH="Authorization: Bearer ${SECRET}"

mgmt_get()  { curl -fsS -H "$AUTH" "$BASE/$1"; }
mgmt_post() { curl -fsS -H "$AUTH" -H 'Content-Type: application/json' -X POST --data "$2" "$BASE/$1"; }

# 1. Current OAuth bindings: [{provider,auth_id}], enabled oauth accounts only.
OAUTH_JSON="$(mgmt_get 'auth-accounts' \
  | jq -c '[.items[] | select(.oauth==true and (.disabled|not)) | {provider, auth_id: .id}]')"
OAUTH_COUNT="$(echo "$OAUTH_JSON" | jq 'length')"
[ "$OAUTH_COUNT" -gt 0 ] || { echo "no enabled OAuth accounts reported by $BASE_URL" >&2; exit 1; }
echo "OAuth accounts to bind ($OAUTH_COUNT):"
echo "$OAUTH_JSON" | jq -r '.[] | "  [\(.provider)] \(.auth_id)"'

# 2. Walk every page of keys into one array.
query_active=""; [ "$ACTIVE_ONLY" -eq 1 ] && query_active="&active_only=1"
page=1; total_pages=1
KEYS_JSON='[]'
while [ "$page" -le "$total_pages" ]; do
  resp="$(mgmt_get "keys?page=${page}&page_size=100${query_active}")"
  total_pages="$(echo "$resp" | jq '.total_pages // 1')"
  KEYS_JSON="$(jq -c --argjson a "$KEYS_JSON" '$a + .items' <<<"$resp")"
  page=$((page + 1))
done
KEY_COUNT="$(echo "$KEYS_JSON" | jq 'length')"
echo "keys found: $KEY_COUNT"

# 3. Bind. Merge = union on (provider,auth_id) lowercased, order-stable; --replace overwrites.
changed=0; skipped=0; failed=0
while IFS= read -r key; do
  id="$(jq -r '.id' <<<"$key")"
  label="$(jq -r '.label // .id' <<<"$key")"
  existing="$(jq -c '.auth_bindings // []' <<<"$key")"

  if [ "$REPLACE" -eq 1 ]; then
    target="$OAUTH_JSON"
  else
    target="$(jq -c -n --argjson e "$existing" --argjson o "$OAUTH_JSON" \
      '($e + $o) | reduce .[] as $b ([]; if any(.[]; (.provider|ascii_downcase)==($b.provider|ascii_downcase) and (.auth_id|ascii_downcase)==($b.auth_id|ascii_downcase)) then . else . + [$b] end)')"
  fi

  before="$(jq -cS 'map({provider,auth_id})|sort' <<<"$existing")"
  after="$(jq -cS 'map({provider,auth_id})|sort' <<<"$target")"
  if [ "$before" = "$after" ]; then skipped=$((skipped+1)); continue; fi

  n="$(jq 'length' <<<"$target")"
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "WOULD bind $label ($id): $n accounts"; changed=$((changed+1)); continue
  fi
  payload="$(jq -c -n --arg id "$id" --argjson b "$target" '{id:$id, auth_bindings:$b}')"
  if mgmt_post 'keys/update' "$payload" >/dev/null; then
    echo "bound $label ($id): $n accounts"; changed=$((changed+1))
  else
    echo "FAILED $label ($id)" >&2; failed=$((failed+1))
  fi
done < <(echo "$KEYS_JSON" | jq -c '.[]')

echo
suffix=""; [ "$DRY_RUN" -eq 1 ] && suffix="  [dry-run]"
echo "done. changed=$changed skipped(up-to-date)=$skipped failed=$failed${suffix}"
[ "$failed" -eq 0 ]
