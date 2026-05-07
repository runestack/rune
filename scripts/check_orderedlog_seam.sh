#!/usr/bin/env bash
# scripts/check_orderedlog_seam.sh
#
# Seam-leakage lint enforcing RUNE-039:
#
# The orderedlog package owns mutations under the protected key prefixes
# below. Anything else that writes those prefixes through Badger directly
# (instead of going through OrderedLog.Propose) breaks the seam and will
# silently diverge the moment the Raft backend lands in Phase 2.
#
# This script greps the source tree for direct Badger writes to those
# prefixes from outside pkg/store/orderedlog/. It exits non-zero on any
# match. Run from the rune/ module root.
#
# Protected prefixes (must match keyPrefix conventions of the networking
# layer):
#   network/        - ClusterNetwork + VIP allocator state (RUNE-040)
#   endpoints/      - ServiceEndpoints / LocalInstances (RUNE-063)
#   policy/         - Compiled network-policy state (RUNE-064)

set -euo pipefail

cd "$(dirname "$0")/.."

PROTECTED_PREFIXES=(
  '"network/'
  '"endpoints/'
  '"policy/'
)

EXCLUDE_DIRS=(
  './pkg/store/orderedlog'   # source of truth — owns the seam
  './scripts'                # this script + future helpers
  './_docs'                  # docs reference the strings
)

EXCLUDE_GREP=""
for d in "${EXCLUDE_DIRS[@]}"; do
  EXCLUDE_GREP+=" --exclude-dir=${d##*/}"
done

bad=0
for prefix in "${PROTECTED_PREFIXES[@]}"; do
  # Look for Badger txn.Set / txn.Delete calls referencing the prefix.
  matches=$(grep -RIn \
    --include='*.go' \
    $EXCLUDE_GREP \
    -E "(txn|tx)\.(Set|Delete)\([^)]*${prefix}" . || true)
  if [ -n "$matches" ]; then
    echo "ERROR: direct Badger write to protected prefix ${prefix}*:"
    echo "$matches"
    echo
    echo "Fix: route this mutation through orderedlog.OrderedLog.Propose."
    bad=1
  fi
done

if [ "$bad" -ne 0 ]; then
  echo
  echo "Seam-leakage lint FAILED. See pkg/store/orderedlog/orderedlog.go"
  echo "for the rule and RUNE-039 in _docs/ENGINEERING_TICKETS.md."
  exit 1
fi

echo "orderedlog seam OK"
