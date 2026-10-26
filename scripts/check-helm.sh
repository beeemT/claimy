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
# --reuse-values from an older release has no annotation maps. Missing maps
# must retain the Helm hooks instead of turning migration into ordinary resources.
for template in migration-configmap migration-job; do
  helm template claimy-check "$chart" --values "$values" \
    --set-json 'migrations.annotations=null' --set-json 'migrations.configMapAnnotations=null' \
    --show-only "templates/$template.yaml" >"$tmp/reused-$template.yaml"
  grep -Fq 'helm.sh/hook: pre-install,pre-upgrade' "$tmp/reused-$template.yaml"
done

# A fresh Argo installation must be able to create its database and Secrets
# before migration, while keeping the API behind a successful migration.
argo_values="$tmp/argo-values.yaml"
cat >"$argo_values" <<'YAML'
migrations:
  configMapAnnotations:
    argocd.argoproj.io/hook: Sync
    argocd.argoproj.io/sync-wave: "0"
    argocd.argoproj.io/hook-delete-policy: BeforeHookCreation
  annotations:
    argocd.argoproj.io/hook: Sync
    argocd.argoproj.io/sync-wave: "1"
    argocd.argoproj.io/hook-delete-policy: BeforeHookCreation,HookSucceeded
deploymentAnnotations:
  argocd.argoproj.io/sync-wave: "2"
YAML
for template in migration-configmap migration-job deployment; do
  helm template claimy-check "$chart" --values "$values" --values "$argo_values" \
    --show-only "templates/$template.yaml" >"$tmp/$template.yaml"
done
for template in migration-configmap migration-job; do
  grep -Fq 'argocd.argoproj.io/hook: Sync' "$tmp/$template.yaml"
done
grep -Fq 'argocd.argoproj.io/sync-wave: "0"' "$tmp/migration-configmap.yaml"
grep -Fxq '    argocd.argoproj.io/hook-delete-policy: BeforeHookCreation' "$tmp/migration-configmap.yaml"
grep -Fq 'argocd.argoproj.io/sync-wave: "1"' "$tmp/migration-job.yaml"
grep -Fq 'argocd.argoproj.io/hook-delete-policy: BeforeHookCreation,HookSucceeded' "$tmp/migration-job.yaml"
grep -Fq 'argocd.argoproj.io/sync-wave: "2"' "$tmp/deployment.yaml"
for annotations in annotations configMapAnnotations; do
  if helm template claimy-check-invalid "$chart" --values "$values" \
    --set "migrations.$annotations.invalid=true" >/dev/null 2>&1; then
    echo "error: non-string migration $annotations were accepted" >&2
    exit 1
  fi
done
printf '%s\n' 'Helm lint, render, package, and Argo sync-wave checks passed'
