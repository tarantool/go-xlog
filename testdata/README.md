# testdata/

Golden fixtures produced by a real Tarantool 3.x instance via
`scripts/gen-fixtures.sh` (which boots Tarantool against
`scripts/gen.lua` in a temp dir, then copies the resulting files here).

Regenerate with:

```
# uses `tarantool` from PATH, or set TARANTOOL_BIN to override
TARANTOOL_BIN=/path/to/tarantool ./scripts/gen-fixtures.sh
```

The fixtures were last generated against
`tarantool 3.8.0-entrypoint-49-g97a3b38040` (format version `0.13`).

## Live integration tests

The static fixtures above are byte snapshots. For end-to-end validation
against a live Tarantool — in both directions — see
`internal/integration/`, gated behind the `tarantool` build tag:

```
go test -tags tarantool ./internal/integration/...
```

- **read**: Tarantool writes an xlog, go-xlog reads it; tuples and
  tx-grouping are asserted against the inserted data.
- **write**: go-xlog writes an xlog (incl. a multi-stmt tx and a
  zstd-compressed block), Tarantool's own `xlog.pairs` reads it back, and
  the decoded rows are asserted against what was written.

The tests skip gracefully if no Tarantool binary is found (set
`TARANTOOL_BIN` or put `tarantool` on `PATH`). Without the `tarantool`
tag the default `go test ./...` ignores the package entirely.

## Historical corpus (`historical/`)

`testdata/historical/<version>/` holds a **frozen** corpus of real artefacts
from a transition-pinned range of Tarantool versions, used by the default-run
`internal/compat/` golden tests (no Tarantool or Docker needed at test time).

| Dir | Format | Exercises |
|---|---|---|
| `2.11` | `0.13` | mature 2.x journal: multi-stmt tx, zstd-compressed blocks, RAFT + SYNCHRO (CONFIRM) rows, vinyl (`.vylog`/`.run`/`.index`) |
| `3.0`  | `0.13` | early 3.x |
| `3.8`  | `0.13` | current HEAD |

Each dir carries a `VERSION` file (exact `tarantool --version`) and preserves
the vinyl `<space>/<index>/` subtree. Regenerate the whole corpus with:

```
PODMAN_HOST=<podman-host> ./scripts/gen-historical.sh
```

The script boots each version (`2.11`/`3.0` from their official images on the
podman host; `3.8` uses the local binary) via `scripts/historical/gen-modern.lua`,
and is never invoked by `go test`.

## Files

| File | Origin | Size | Contains |
|---|---|---|---|
| `empty.snap` | bootstrap snap, signature `0` | 5116 B | initial system spaces only, `VClock: {}` |
| `populated.snap` | post-`box.snapshot()` snap, signature `12` | 5320 B | system spaces + test/vtest user spaces + their rows, `VClock: {1: 9}` (memtx) / `{1: 12}` (counting vinyl) — see header |
| `simple.xlog` | pre-snapshot xlog, signature `0` | 858 B | schema rows + 3 single-stmt inserts + 1 multi-row tx (3 stmts) + 1 large-tuple insert (zstd-compressed tx) + 1 vinyl insert |
| `multistmt.xlog` | identical content to `simple.xlog` | 858 B | named distinctly so `format` tests can refer to the multi-row tx without coupling to the simple-tx test |
| `compressed.xlog` | identical content to `simple.xlog` | 858 B | named distinctly so `format` tests can refer to the zstd-marked tx without coupling |
| `vylog_sample.vylog` | initial vylog, signature `0` | 388 B | LSM/range/run records from creating the `vtest` vinyl space and dumping it on `box.snapshot()` |

All three xlog aliases point to the same underlying bytes because all the
DML lands in a single pre-snapshot xlog. The duplication is intentional —
tests can name the fixture by intent (`simple.xlog` for header-shape
checks, `multistmt.xlog` for tx-grouping checks, `compressed.xlog` for
the zstd-marker check) without exploding the corpus.

## Magic-byte inventory in `simple.xlog`

```
offset  marker
   110  D5 BA 0B AB  ROW   (uncompressed tx)
   171  D5 BA 0B AB  ROW
   247  D5 BA 0B AB  ROW
   309  D5 BA 0B AB  ROW
   462  D5 BA 0B AB  ROW
   512  D5 BA 0B AB  ROW
   561  D5 BA 0B AB  ROW
   611  D5 BA 0B AB  ROW
   736  D5 BA 0B BA  ZROW  (zstd-compressed tx — the 4 KiB tuple)
   800  D5 BA 0B AB  ROW
   854  D5 10 AD ED  EOF
```

Confirms the ROW/ZROW magic bytes and that the compress threshold (2 KiB)
was crossed by the 4 KiB-tuple insert.
