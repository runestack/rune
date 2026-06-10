#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT_DIR"

# The harness builds runed + rune itself (once per test process), so no
# pre-build step is needed here. Readiness window is env-tunable for
# slow CI runners.
export RUNE_E2E_HEALTH_TIMEOUT_SECONDS=${RUNE_E2E_HEALTH_TIMEOUT_SECONDS:-60}

echo "[E2E] Running tests with -tags=e2e (real runed server)"
go test ./test/e2e/... -tags=e2e -v -timeout 20m "$@"
