#!/usr/bin/env bash
# Generate the frozen historical-compatibility fixture corpus for go-xlog.
#
# Boots a range of Tarantool versions, each producing real .xlog/.snap/.vylog/
# .run/.index artefacts, and copies them into testdata/historical/<tag>/.
#
# The containerised versions (2.11/3.0) run their official amd64 images on an
# ssh-reachable podman host. The current line (3.8) runs against the local
# `tarantool` binary on PATH.
#
# This script is NEVER invoked by `go test` — the committed corpus is what the
# tests read. Re-run it only to regenerate the corpus (PC3).
#
# Usage:
#   PODMAN_HOST=<podman-host> ./scripts/gen-historical.sh
#
# Env:
#   PODMAN_HOST   ssh host with podman for the containerised versions (required).
#   TARANTOOL_BIN local binary for the 3.8 row. Default: tarantool

set -euo pipefail

PODMAN_HOST="${PODMAN_HOST:?set PODMAN_HOST to an ssh host with podman for the containerised versions}"
TARANTOOL_BIN="${TARANTOOL_BIN:-tarantool}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CORPUS_DIR="${REPO_ROOT}/testdata/historical"
MODERN_LUA="${SCRIPT_DIR}/historical/gen-modern.lua"

# find predicate matching the five on-disk artefact types.
art_files() { find "$1" -type f \( -name '*.snap' -o -name '*.xlog' -o -name '*.vylog' -o -name '*.run' -o -name '*.index' \); }

# Matrix: "<dir-tag> <source>"
#   source = an image tag (run on $PODMAN_HOST) or "local" (run $TARANTOOL_BIN)
MATRIX=(
    "2.11  2.11"
    "3.0   3.0"
    "3.8   local"
)

# Collect the five artefact types from a directory into the corpus dir,
# preserving the vinyl <space>/<index>/ subtree.
# Remote find predicate (literal, runs on $PODMAN_HOST).
REMOTE_FIND="find . -type f \( -name '*.snap' -o -name '*.xlog' -o -name '*.vylog' -o -name '*.run' -o -name '*.index' \)"

collect_local() {
    local from="$1" to="$2"
    rm -rf "$to"; mkdir -p "$to"
    art_files "$from" | sed "s#^${from}/##" | tar cf - -C "$from" -T - | tar xf - -C "$to"
}

gen_remote() {
    local tag="$1" gen_lua="$2" out="$3"
    local ver rd
    ver="$(ssh "${PODMAN_HOST}" "podman run --rm --entrypoint tarantool docker.io/tarantool/tarantool:${tag} --version 2>/dev/null | head -1")"
    rd="$(ssh "${PODMAN_HOST}" mktemp -d)"
    scp -q "${gen_lua}" "${PODMAN_HOST}:${rd}/gen.lua"
    # Clear the 3.x image's baked TT_* config vars so box.cfg runs in imperative
    # mode. No trailing `; echo` — the sh -c must exit with tarantool's status so
    # a generation failure propagates through podman/ssh to the outer set -e.
    ssh "${PODMAN_HOST}" "podman run --rm -i -v ${rd}:/wd:Z --entrypoint /bin/sh docker.io/tarantool/tarantool:${tag} -c 'unset TT_INSTANCE_NAME TT_APP_NAME TT_CONFIG; cd /wd && tarantool /wd/gen.lua >/wd/run.out 2>&1'"
    rm -rf "${out}"; mkdir -p "${out}"
    ssh "${PODMAN_HOST}" "cd ${rd} && ${REMOTE_FIND} | tar cf - -T -" | tar xf - -C "${out}"
    ssh "${PODMAN_HOST}" "rm -rf ${rd}"
    printf '%s\n' "${ver}" > "${out}/VERSION"
}

gen_local() {
    local gen_lua="$1" out="$2"
    local ver wd
    ver="$(${TARANTOOL_BIN} --version | head -1)"
    wd="$(mktemp -d -t go-xlog-hist.XXXXXX)"
    ( cd "${wd}" && "${TARANTOOL_BIN}" "${gen_lua}" "${wd}" >/dev/null 2>&1 )
    collect_local "${wd}" "${out}"
    rm -rf "${wd}"
    printf '%s\n' "${ver}" > "${out}/VERSION"
}

echo "==> corpus dir: ${CORPUS_DIR}"
for row in "${MATRIX[@]}"; do
    read -r dir source <<<"${row}"
    out="${CORPUS_DIR}/${dir}"
    echo "==> ${dir} (source=${source})"
    if [ "${source}" = local ]; then
        gen_local "${MODERN_LUA}" "${out}"
    else
        gen_remote "${source}" "${MODERN_LUA}" "${out}"
    fi
    echo "    $(cat "${out}/VERSION")"
    find "${out}" -type f ! -name VERSION | sed "s#${out}/#      #" | sort
done
echo "==> done."
