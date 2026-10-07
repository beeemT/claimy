#!/bin/sh
set -eu

version=${1:-0.0.0}
case "$version" in
  v*) version=${version#v} ;;
esac
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || {
  echo 'error: chart version must be MAJOR.MINOR.PATCH' >&2
  exit 2
}

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out=${2:-$root/dist}
command -v helm >/dev/null 2>&1 || { echo 'error: helm is required' >&2; exit 1; }
mkdir -p "$out"
helm package "$root/build/helm/claimy" \
  --destination "$out" \
  --version "$version" \
  --app-version "$version"
printf 'Helm chart written to %s/claimy-chart-%s.tgz\n' "$out" "$version"
