#!/usr/bin/env bash
# Standard pre-push check for go-xlog.
#
#   ./scripts/test.sh          # full suite under the race detector (runs fuzz
#                              # seeds + committed crashers as ordinary tests)
#   ./scripts/test.sh --fuzz   # + a short active fuzz burst per parser target
#                              # (FUZZTIME overrides the per-target duration)

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

echo "==> go test -race ./..."
go test -race ./...

if [[ "${1:-}" == "--fuzz" ]]; then
    dur="${FUZZTIME:-15s}"
    echo "==> active fuzz burst (${dur} per target)"
    # Go runs at most one fuzz target per invocation.
    for t in format:FuzzDecodeMeta format:FuzzDecodeXRow format:FuzzDecodeTxBlock reader:FuzzReader; do
        pkg="${t%%:*}"; fn="${t##*:}"
        echo "    -- ${pkg} ${fn}"
        go test "./${pkg}/" -run '^$' -fuzz "^${fn}$" -fuzztime="${dur}"
    done
fi

echo "==> ok"
