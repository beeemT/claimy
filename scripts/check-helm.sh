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
  host: ""
  port: 3306
  name: claimy
  user: claimy
  existingSecret: claimy-db
  passwordKey: password
  parameters:
    loc: UTC
    time_zone: "'+00:00'"
    parseTime: "true"
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
        host: ""
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
      team_domain: ""
      rest:
        issuer: https://issuer.example.test
        audience: ""
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
      app_identity: ""
      allowed_spaces: []
      deadline: 25s
service:
  port: 8088
migrations:
  enabled: true
  backoffLimit: 6
  activeDeadlineSeconds: 600
  resources: {}
extraEnv:
  - name: SQLC_DEFAULT_URI_HOST
    valueFrom:
      secretKeyRef:
        name: claimy-runtime
        key: host
  - name: CLAIMY_AUTH_TEAM_DOMAIN
    valueFrom:
      secretKeyRef:
        name: claimy-runtime
        key: team-domain
  - name: CLAIMY_AUTH_REST_AUDIENCE
    valueFrom:
      configMapKeyRef:
        name: claimy-runtime
        key: rest-audience
  - name: CLAIMY_CHAT_APP_IDENTITY
    valueFrom:
      secretKeyRef:
        name: claimy-runtime
        key: chat-identity
  - name: CLAIMY_CHAT_ALLOWED_SPACES
    valueFrom:
      secretKeyRef:
        name: claimy-runtime
        key: chat-spaces
  - name: POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
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

if helm template claimy-check-invalid "$chart" --values "$values" --set database.parameters.loc=localtime >/dev/null 2>&1; then
  echo 'error: invalid database timezone was accepted' >&2
  exit 1
fi
if helm template claimy-check-invalid "$chart" --values "$values" --set database.host=bad_host >/dev/null 2>&1; then
  echo 'error: invalid literal database host was accepted' >&2
  exit 1
fi
if helm template claimy-check-invalid "$chart" --values "$values" --set-json 'extraEnv=[]' >/dev/null 2>&1; then
  echo 'error: empty metadata values were accepted without their runtime references' >&2
  exit 1
fi
if helm template claimy-check-invalid "$chart" --values "$values" --set 'extraEnv[0].name=UNRELATED' >/dev/null 2>&1; then
  echo 'error: unrelated environment reference satisfied a required config value' >&2
  exit 1
fi
if helm template claimy-check-invalid "$chart" --values "$values" --set 'extraEnv[0].valueFrom.secretKeyRef.optional=true' >/dev/null 2>&1; then
  echo 'error: optional Secret reference satisfied a required config value' >&2
  exit 1
fi
if helm template claimy-check-invalid "$chart" --values "$values" --set config.claimy.auth.team_domain=bad_domain >/dev/null 2>&1; then
  echo 'error: invalid literal authentication domain was accepted' >&2
  exit 1
fi
printf '%s\n' 'Helm lint, render, and package checks passed'
