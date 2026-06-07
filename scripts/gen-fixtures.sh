#!/usr/bin/env bash
# Generate Tarantool 3.x golden fixtures for go-xlog tests.
#
# Usage:
#   TARANTOOL_BIN=/path/to/tarantool ./scripts/gen-fixtures.sh
#
# Defaults TARANTOOL_BIN to "tarantool" on PATH.
#
# The script is re-runnable: it always works in a fresh temp dir and
# overwrites the canonical fixture names in testdata/.

set -euo pipefail

TARANTOOL_BIN="${TARANTOOL_BIN:-tarantool}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TESTDATA_DIR="${REPO_ROOT}/testdata"
LUA_SCRIPT="${SCRIPT_DIR}/gen.lua"

if ! command -v "${TARANTOOL_BIN}" >/dev/null 2>&1; then
    echo "error: tarantool binary not found: ${TARANTOOL_BIN}" >&2
    exit 1
fi

WORK_DIR="$(mktemp -d -t go-xlog-fixtures.XXXXXX)"
cleanup() {
    rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

echo "==> using tarantool: ${TARANTOOL_BIN}"
echo "==> work dir:        ${WORK_DIR}"
echo "==> testdata dir:    ${TESTDATA_DIR}"

mkdir -p "${TESTDATA_DIR}"

# Run the lua generator. It calls box.cfg{ work_dir = arg[1] } so all
# artefacts (snap, xlog, vylog) land in ${WORK_DIR}.
"${TARANTOOL_BIN}" "${LUA_SCRIPT}" "${WORK_DIR}"

echo "==> generated files (recursive):"
find "${WORK_DIR}" -maxdepth 2 -type f -printf '%s\t%p\n' 2>/dev/null \
    || find "${WORK_DIR}" -maxdepth 2 -type f -exec ls -la {} +

shopt -s nullglob
XLOGS=( "${WORK_DIR}"/*.xlog )
SNAPS=( "${WORK_DIR}"/*.snap )
# Tarantool 3.x may place vylog in WORK_DIR or in a vinyl subdir.
VYLOGS=( "${WORK_DIR}"/*.vylog "${WORK_DIR}"/vinyl/*.vylog )

if (( ${#XLOGS[@]} == 0 )); then
    echo "error: no .xlog files produced" >&2
    exit 1
fi
if (( ${#SNAPS[@]} == 0 )); then
    echo "error: no .snap files produced" >&2
    exit 1
fi

# Pick the xlog with the most content (largest). The post-snapshot xlog is
# typically tiny (a few schema rows), whereas the pre-snapshot xlog holds
# our inserts + multi-row tx + 4 KiB tuple.
SRC_XLOG=""
SRC_XLOG_SIZE=0
for f in "${XLOGS[@]}"; do
    sz=$(wc -c < "$f" | tr -d ' ')
    if (( sz > SRC_XLOG_SIZE )); then
        SRC_XLOG="$f"
        SRC_XLOG_SIZE=$sz
    fi
done

echo "==> picked xlog: ${SRC_XLOG} (${SRC_XLOG_SIZE} bytes)"

cp -f "${SRC_XLOG}" "${TESTDATA_DIR}/simple.xlog"
cp -f "${SRC_XLOG}" "${TESTDATA_DIR}/multistmt.xlog"
cp -f "${SRC_XLOG}" "${TESTDATA_DIR}/compressed.xlog"

# Snaps: sort by name (signature is a zero-padded prefix; lexicographic sort
# matches numeric sort). First = empty (signature 0), last = populated.
IFS=$'\n' SNAPS_SORTED=( $(printf '%s\n' "${SNAPS[@]}" | sort) )
unset IFS

cp -f "${SNAPS_SORTED[0]}" "${TESTDATA_DIR}/empty.snap"
cp -f "${SNAPS_SORTED[-1]}" "${TESTDATA_DIR}/populated.snap"

if (( ${#VYLOGS[@]} > 0 )); then
    # Pick the largest (most content).
    SRC_VYLOG=""
    SRC_VYLOG_SIZE=0
    for f in "${VYLOGS[@]}"; do
        sz=$(wc -c < "$f" | tr -d ' ')
        if (( sz >= SRC_VYLOG_SIZE )); then
            SRC_VYLOG="$f"
            SRC_VYLOG_SIZE=$sz
        fi
    done
    cp -f "${SRC_VYLOG}" "${TESTDATA_DIR}/vylog_sample.vylog"
    echo "==> picked vylog: ${SRC_VYLOG} (${SRC_VYLOG_SIZE} bytes)"
else
    echo "warning: no .vylog produced (vinyl space may not have been created)" >&2
fi

echo "==> copied fixtures:"
ls -la "${TESTDATA_DIR}"
echo "==> done."
