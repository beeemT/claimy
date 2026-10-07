#!/bin/sh
set -eu

version=${1:-0.0.0}
case "$version" in
  v*) version=${version#v} ;;
esac
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || {
  echo 'error: version must be MAJOR.MINOR.PATCH' >&2
  exit 2
}

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
dist="$root/dist"
mkdir -p "$dist"
command -v go >/dev/null 2>&1 || { echo 'error: go is required' >&2; exit 1; }
[ -f "$root/LICENSE" ] || { echo 'error: LICENSE is missing' >&2; exit 1; }
[ -f "$root/skills/claimy/SKILL.md" ] || { echo 'error: skills/claimy/SKILL.md is missing' >&2; exit 1; }

for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  os=${target%/*}
  arch=${target#*/}
  name="claimy_${version}_${os}_${arch}"
  stage=$(mktemp -d "${TMPDIR:-/tmp}/claimy-package.XXXXXX")
  trap 'rm -rf "$stage"' EXIT HUP INT TERM
  echo "==> building $name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "$stage/claimy" ./cmd/claimy
  cp "$root/skills/claimy/SKILL.md" "$stage/SKILL.md"
  cp "$root/LICENSE" "$stage/LICENSE"
  COPYFILE_DISABLE=1 tar -C "$stage" -czf "$dist/$name.tar.gz" claimy SKILL.md LICENSE
  rm -rf "$stage"
done

printf 'archives written to %s\n' "$dist"
