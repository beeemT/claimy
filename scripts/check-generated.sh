#!/bin/sh
set -eu

go generate ./...

git diff --exit-code -- api/openapi.yaml pkg/client/client.gen.go ':(glob)internal/**/mocks/**'
untracked=$(git ls-files --others --exclude-standard -- api/openapi.yaml pkg/client/client.gen.go ':(glob)internal/**/mocks/**')
if [ -n "$untracked" ]; then
    printf 'Generated files are not tracked:\n%s\n' "$untracked" >&2
    exit 1
fi
