#!/usr/bin/env bash
# Offline checks used locally and by CI: lint, format check and tests (the
# tests use a fake engine; they never touch the network).
set -euo pipefail
cd "$(dirname "$0")/.."
uv sync --frozen --all-groups
uv run ruff check src tests
uv run ruff format --check src tests
uv run pytest -q
echo "all checks passed"
