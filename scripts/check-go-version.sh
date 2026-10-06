#!/bin/sh
set -eu

go_mod_version=$(awk '
  $1 == "go" {
    count++
    if (NF != 2) {
      malformed = 1
    } else {
      version = $2
    }
  }
  END {
    if (malformed || count != 1) {
      exit 1
    }
    print version
  }
' go.mod) || {
  printf '%s\n' 'error: expected exactly one simple Go version directive in go.mod' >&2
  exit 1
}

mise_go_version=$(awk '
  /^[[:space:]]*\[tools\][[:space:]]*(#.*)?$/ {
    in_tools = 1
    next
  }
  /^[[:space:]]*\[/ {
    in_tools = 0
  }
  in_tools {
    line = $0
    sub(/#.*/, "", line)
    if (line ~ /^[[:space:]]*go[[:space:]]*=/) {
      count++
      sub(/^[[:space:]]*go[[:space:]]*=[[:space:]]*/, "", line)
      if (line !~ /^"[^\"]+"[[:space:]]*$/) {
        malformed = 1
      } else {
        sub(/^"/, "", line)
        sub(/"[[:space:]]*$/, "", line)
        version = line
      }
    }
  }
  END {
    if (malformed || count != 1) {
      exit 1
    }
    print version
  }
' mise.toml) || {
  printf '%s\n' 'error: expected exactly one quoted Go tool version in mise.toml [tools]' >&2
  exit 1
}

if [ "$go_mod_version" != "$mise_go_version" ]; then
  printf 'error: Go version mismatch: go.mod=%s mise.toml=%s\n' "$go_mod_version" "$mise_go_version" >&2
  exit 1
fi

for tool_module in \
  'mockery github.com/vektra/mockery/v3' \
  'oapi-codegen github.com/oapi-codegen/oapi-codegen/v2' \
  'go:github.com/pressly/goose/v3/cmd/goose github.com/pressly/goose/v3'
do
  set -- $tool_module
  pinned=$(mise current "$1")
  module_version=$(go list -m -f '{{.Version}}' "$2")
  if [ "$module_version" != "v$pinned" ]; then
    printf 'error: %s version mismatch: go.mod=%s mise=%s\n' "$1" "$module_version" "$pinned" >&2
    exit 1
  fi
done
