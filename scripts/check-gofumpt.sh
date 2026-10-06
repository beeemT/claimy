#!/bin/sh
set -eu

files=$(gofumpt -l .)
if [ -n "$files" ]; then
  printf 'Go files need gofumpt formatting:\n%s\n' "$files" >&2
  exit 1
fi
