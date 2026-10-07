#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
chart="$root/build/helm/claimy"
command -v helm >/dev/null 2>&1 || { echo 'error: helm is required' >&2; exit 1; }
[ -f "$chart/Chart.yaml" ] || { echo 'error: Helm chart is missing' >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/claimy-helm-check.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
values="$tmp/values.yaml"
rendered="$tmp/rendered.yaml"
cat >"$values" <<'YAML'
nameOverride: claimy
image:
  repository: example.invalid/claimy
  tag: synthetic
  digest: ""
  pullPolicy: IfNotPresent
database:
  host: mysql.claimy.test
  port: 3306
  name: claimy
  user: claimy
  existingSecret: claimy-db
  passwordKey: password
  parameters:
    loc: UTC
    time_zone: "'+00:00'"
config:
  app:
    env: test
    project: claimy
    family: claimy
    name: claimy
  tracing:
    provider: noop
  sampling:
    enabled: false
  metric:
    enabled: false
  httpserver:
    default:
      port: "8088"
  sqlc:
    default:
      driver: mysql
      uri:
        host: mysql.claimy.test
        port: 3306
        user: claimy
        database: claimy
      parameters:
        loc: UTC
        time_zone: "'+00:00'"
      migrations:
        enabled: false
        path: build/migrations/claimy
  claimy:
    auth:
      team_domain: example.test
      rest:
        issuer: https://issuer.example.test
        audience: claimy-test
        jwks_url: https://keys.example.test/rest
      gitlab:
        issuer: https://gitlab.example.test
        audience: claimy-test
        jwks_url: https://keys.example.test/gitlab
      chat:
        issuer: https://accounts.google.com
        audience: claimy-chat-test
        jwks_url: https://keys.example.test/chat
      cli:
        enabled: false
        client_id: claimy-test
        scopes:
          - openid
          - email
        authorization_params: {}
    chat:
      app_identity: claimy-test
      allowed_spaces:
        - spaces/claimy-test
      deadline: 25s
service:
  port: 8088
migrations:
  enabled: true
  backoffLimit: 6
  activeDeadlineSeconds: 600
  resources: {}
YAML

helm lint "$chart" --values "$values"
helm template claimy-check "$chart" --values "$values" >"$rendered"
helm package "$chart" --destination "$tmp" >/dev/null
[ -s "$tmp/claimy-chart-"*.tgz ] || { echo 'error: Helm package did not create an archive' >&2; exit 1; }
# The database password is deliberately sourced from a Secret and must never be
# present in the rendered ConfigMap or migration command as a literal value.
if grep -Fq 'development-only' "$rendered" || grep -Fq 'synthetic-password' "$rendered"; then
  echo 'error: rendered Helm output contains a literal database password' >&2
  exit 1
fi
printf '%s\n' 'Helm lint, render, and package checks passed'
