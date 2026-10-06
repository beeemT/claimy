#!/usr/bin/env bash
set +x
set -u

fail() {
  printf '%s\n' "$1" >&2
  exit 2
}

[[ -n "${CLAIMY_URL:-}" ]] || fail 'CLAIMY_URL is required'
[[ -n "${CLAIMY_ID_TOKEN:-}" ]] || fail 'CLAIMY_ID_TOKEN is required'
[[ -n "${CLAIMY_GROUP:-}" ]] || fail 'CLAIMY_GROUP is required'
[[ -n "${CLAIMY_REQUEST_ID:-}" ]] || fail 'CLAIMY_REQUEST_ID is required'
[[ -n "${CLAIMY_ENVIRONMENTS:-}" ]] || fail 'CLAIMY_ENVIRONMENTS is required'
command -v jq >/dev/null 2>&1 || fail 'jq is required'
command -v curl >/dev/null 2>&1 || fail 'curl is required'
command -v mktemp >/dev/null 2>&1 || fail 'mktemp is required'

response=$(mktemp) || exit 2
trap 'rm -f "$response"' EXIT
payload=$(jq -cn \
  --arg group "$CLAIMY_GROUP" \
  --arg app "${CLAIMY_APP:-}" \
  --arg expiry "${CLAIMY_EXPIRES_AT:-}" \
  --arg requestId "$CLAIMY_REQUEST_ID" \
  --argjson environments "$CLAIMY_ENVIRONMENTS" \
  '{group:$group,environments:$environments,requestId:$requestId}
   + (if $app == "" then {} else {app:$app} end)
   + (if $expiry == "" then {} else {expiresAt:$expiry} end)') || exit 2
url=${CLAIMY_URL%/}/v1/claims/acquire
status=$(printf 'header = "Authorization: Bearer %s"\n' "$CLAIMY_ID_TOKEN" |
  curl --silent --show-error --config - --request POST \
    --header 'Content-Type: application/json' --data-binary "$payload" \
    --output "$response" --write-out '%{http_code}' "$url") || exit 2
[[ "$status" == 200 ]] || { printf 'Claim request failed: HTTP %s\n' "$status" >&2; exit 2; }
jq -e '
  if .acquired == true then
    (.claim | type) == "object"
    and (.claim.activeNow | type) == "boolean"
    and (.claim.id | type) == "string"
    and (.claim.id | length) > 0
    and ((.conflicts // []) | type) == "array"
    and ((.conflicts // []) | length) == 0
  elif .acquired == false then
    (has("claim") | not)
    and (.conflicts | type) == "array"
    and (.conflicts | length) > 0
  else false end
' "$response" >/dev/null || exit 2
if jq -e '.acquired == false or .claim.activeNow == false' "$response" >/dev/null; then exit 1; fi
jq -er '.claim.id' "$response" || exit 2
exit 0
