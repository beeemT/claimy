#!/usr/bin/env bash
set +x
set -u

fail() {
  printf '%s\n' "$1" >&2
  exit 2
}

[[ -n "${CLAIMY_URL:-}" ]] || fail 'CLAIMY_URL is required'
[[ -n "${CLAIMY_ID_TOKEN:-}" ]] || fail 'CLAIMY_ID_TOKEN is required'
[[ -n "${CLAIMY_CLAIM_ID:-}" ]] || fail 'CLAIMY_CLAIM_ID is required'
[[ "$CLAIMY_CLAIM_ID" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] || fail 'CLAIMY_CLAIM_ID must be a UUID'
command -v jq >/dev/null 2>&1 || fail 'jq is required'
command -v curl >/dev/null 2>&1 || fail 'curl is required'
command -v mktemp >/dev/null 2>&1 || fail 'mktemp is required'

request_id=${CLAIMY_RELEASE_REQUEST_ID:-release-${CI_JOB_ID:-manual}-$(date +%s)}
payload=$(jq -cn --arg requestId "$request_id" '{requestId:$requestId}') || exit 2
response=$(mktemp) || exit 2
trap 'rm -f "$response"' EXIT
url=${CLAIMY_URL%/}/v1/claims/${CLAIMY_CLAIM_ID}/release
status=$(printf 'header = "Authorization: Bearer %s"\n' "$CLAIMY_ID_TOKEN" |
  curl --silent --show-error --config - --request POST \
    --header 'Content-Type: application/json' --data-binary "$payload" \
    --output "$response" --write-out '%{http_code}' "$url") || exit 2
[[ "$status" == 200 ]] || { printf 'Claim release failed: HTTP %s\n' "$status" >&2; exit 2; }
jq -e '(.changed | type) == "boolean" and (.claim.id | type) == "string" and (.claim.id | length) > 0' "$response" >/dev/null || exit 2
exit 0
