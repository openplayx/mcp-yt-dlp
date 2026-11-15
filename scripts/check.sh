#!/usr/bin/env bash
# Offline checks used locally and by CI: formatting, vet and tests (the
# server tests use a fake engine and never touch the network). Set YTDLP_BIN
# to also run the integration test against a real yt-dlp and a local server.
set -euo pipefail
cd "$(dirname "$0")/.."
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "gofmt needed:" >&2
  echo "$unformatted" >&2
  exit 1
fi
go vet ./...
go test -race -count=1 ./...
echo "all checks passed"
