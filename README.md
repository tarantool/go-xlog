# go-xlog

[![CI](https://github.com/tarantool/go-xlog/actions/workflows/ci.yml/badge.svg)](https://github.com/tarantool/go-xlog/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tarantool/go-xlog.svg)](https://pkg.go.dev/github.com/tarantool/go-xlog)
[![Go Report Card](https://goreportcard.com/badge/github.com/tarantool/go-xlog)](https://goreportcard.com/report/github.com/tarantool/go-xlog)
[![License: BSD-2-Clause](https://img.shields.io/badge/License-BSD%202--Clause-blue.svg)](LICENSE)

A pure-Go library for reading, writing, and rewriting
[Tarantool](https://www.tarantool.io/) journal files — write-ahead logs
(`.xlog`), snapshots (`.snap`), and vinyl logs (`.vylog`) — without linking the
Tarantool C runtime.

```go
import "github.com/tarantool/go-xlog/reader"

r, err := reader.Open("00000000000000000000.xlog")
if err != nil {
    log.Fatal(err)
}
defer r.Close()

for tx, err := range r.Txs() {
    if err != nil {
        log.Fatal(err)
    }
    for _, row := range tx.Rows {
        fmt.Printf("lsn=%d type=%d\n", row.LSN, row.Type)
    }
}
```

## Why

Tarantool stores its WAL and snapshots in a self-describing binary format: a
text header followed by a chain of CRC-checked, optionally zstd-compressed
transaction blocks. The only complete reference is the Tarantool C source.
go-xlog reimplements that codec in Go so you can build backup/restore tooling,
replication shims, log inspectors, and migration scripts against
`.xlog`/`.snap`/`.vylog` files directly — no cgo, no running instance.

The library is format-faithful to Tarantool 3.x (format version `0.13`) and
reads legacy `0.12` files with an opt-in flag. The on-disk layout, CRC algorithm
(CRC32C / Castagnoli), msgpack row encoding, and compression threshold all
mirror the C implementation byte-for-byte, and the test suite validates against
a frozen corpus of real files produced by Tarantool 2.11 through 3.8.

## Install

```sh
go get github.com/tarantool/go-xlog
```

Requires Go 1.24+ (the reader exposes `iter.Seq2` range-over-func iterators).

## Concepts

A journal **file** is a text `Meta` header plus a sequence of
**transaction blocks**. Each block is one logical transaction containing one or
more **rows** (`XRow`). Rows carry an LSN (log sequence number) and, for
multi-statement transactions, a shared TSN (transaction sequence number); the
terminating row sets a commit flag.

Files within a directory form a **chain**: each is named after the *signature*
of its vector clock (`VClock`, the per-replica LSN map), and each file's
`PrevVClock` equals the previous file's `VClock`. This chaining lets you locate
which file contains a given LSN or vclock without scanning every file.

The packages are layered so you can drop in at the level you need:

| Package | Role |
| --- | --- |
| [`format`](#package-format) | Pure byte-level codec — `Meta`, `XRow`, `VClock`, encode/decode. No I/O. |
| [`reader`](#package-reader) | Single-file forward cursor: row- and transaction-level iteration. |
| [`writer`](#package-writer) | Single-file write-once cursor with atomic finalize. |
| [`filter`](#package-filter) | Composable row predicates (`And`/`Or`/`Not`, by replica, type, LSN range). |
| [`tools`](#package-tools) | Meta-only rewrites that preserve payload bytes and CRCs. |

## Usage

### Reading

`reader` gives you a forward-only cursor over a single file. Open it depending
on who owns the underlying stream and where the bytes live:

```go
r, err := reader.Open("data.xlog")                 // Reader owns the file.
r, err := reader.OpenFS(fsys, "data.xlog")         // Read from an fs.FS.
r, err := reader.NewReader(readSeeker)             // Caller owns the stream.
r, err := reader.OpenMmap("data.xlog")             // Memory-mapped, zero-copy.
r, err := reader.NewReaderBytes(buf)               // A journal already in memory.
```

Inspect the header via `Meta()`, then iterate at whichever granularity fits:

```go
r, _ := reader.Open("data.xlog")
defer r.Close()

meta := r.Meta()
fmt.Println(meta.Filetype, meta.Version, meta.VClock)

// Row at a time:
for {
    row, err := r.Next()
    if err == io.EOF {
        break
    }
    if err != nil {
        log.Fatal(err)
    }
    // ... use row
}

// Or transaction at a time (rows grouped by TSN):
for {
    tx, err := r.NextTx()
    if err == io.EOF {
        break
    }
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("tx starting at lsn=%d, %d rows\n", tx.StartLSN, len(tx.Rows))
}
```

Range-over-func equivalents, `Rows()` and `Txs()`, surface errors as the second
loop variable:

```go
for row, err := range r.Rows() {
    if err != nil { log.Fatal(err) }
    // ...
}
```

**Reader options:**

- `SkipCorruptTx()` — on a CRC mismatch or unknown magic, scan forward to the
  next valid block instead of erroring. For salvaging damaged files.
- `IgnoreMissingEOF()` — treat a missing EOF marker as a clean end of stream.
  Useful when tailing a file an instance is still writing.
- `AcceptV012()` — accept the legacy `0.12` format-version line in the meta
  header (the current format is `0.13`). The legacy `Server:` alias for the
  `Instance:` UUID line is accepted regardless of this option.
- `WithAliasBodies()` — let `Row().BodyRaw` alias the read buffer instead of
  copying it (see the zero-allocation cursor below).

Mid-stream failures map to sentinel errors you can test with `errors.Is`:
`ErrTruncated`, `ErrCorruptCRC`, `ErrUnknownMagic`, `ErrTxTooLarge`,
`ErrZeroLengthDecode`.

#### Zero-allocation cursor

For throughput-sensitive consumers, `Scan`/`ScanTx` decode rows into
reader-owned scratch instead of allocating per row. `Row()` (and `Tx()`) return
the current row(s); check `Err()` after the loop.

```go
for r.Scan() {
    row := r.Row()
    // ... use row
}
if err := r.Err(); err != nil { log.Fatal(err) }
```

By default each row's body is copied into a reader arena, so rows stay valid
until you call `Recycle()` — accumulate a batch, process it, `Recycle()` to
reclaim the arena, repeat. With `WithAliasBodies()` the body instead aliases the
read buffer (valid only until the next `Scan`), for the fastest streaming path
when you fully consume each row before advancing. Paired with
`OpenMmap`/`NewReaderBytes`, aliased bodies are zero-copy *and* stay valid for
the reader's lifetime, since the backing buffer outlives the cursor.

#### Verbatim block access

`NextBlockRaw()` returns the next physical transaction block's on-disk bytes
(fixheader + payload) verbatim, CRC-verified, without decoding any rows — the
read half of the verbatim-copy fast path (see
[`pipe.CopyRaw`](#filtering-and-copying)).

### Writing

`writer` produces one well-formed file. `Create` writes to `<path>.inprogress`
and atomically renames it to the final name on `Close`, so readers never observe
a partial file. `NewWriter` writes to any `io.Writer` (e.g. a `bytes.Buffer`)
without the rename dance.

```go
meta := &format.Meta{
    Filetype:     format.FiletypeXLOG,
    InstanceUUID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
    VClock:       format.VClock{1: 0},
}

w, err := writer.Create("out.xlog", meta)
if err != nil {
    log.Fatal(err)
}

// Accumulate rows into the pending tx, then commit them as one block:
w.WriteRow(format.XRow{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: body1})
w.WriteRow(format.XRow{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: body2})
w.CommitTx()

// Or write a whole transaction in one call:
w.WriteTx([]format.XRow{
    {Type: iproto.IPROTO_REPLACE, LSN: 3, BodyRaw: body3},
})

if err := w.Close(); err != nil { // EOF marker, fsync, atomic rename.
    log.Fatal(err)
}
```

`XRow` is passed and returned **by value** throughout the read/write API; slices
(`[]format.XRow`) keep the read→write pipeline copy-free. The single-row
`WriteRow`/`Next` pay only a small struct copy.

Request types, IPROTO header/body keys, and flag bits come from
[`github.com/tarantool/go-iproto`](https://github.com/tarantool/go-iproto) —
`XRow.Type` is an `iproto.Type` and `XRow.Flags` an `iproto.Flag`, so you write
`iproto.IPROTO_INSERT`, `iproto.IPROTO_FLAG_COMMIT`, etc. `format.TypeName`
renders a type as a short string (`"INSERT"`).

Transaction blocks larger than 2 KiB are zstd-compressed automatically (matching
Tarantool's threshold and level). Use `Discard()` instead of `Close()` to
abandon the `.inprogress` file without promoting it.

**Writer options:**

- `WithCompression(format.Compression{...})` — set the block compression policy:
  `Disabled` (never compress), `Level` (zstd level, default 3), `Threshold` (min
  payload bytes to compress, default 2 KiB). `NoCompression()` is sugar for
  `Disabled: true`.
- `Version(s string)` — set `Meta.Version` if the caller left it blank.
- `Sync(mode)` — `SyncNormal` (default, `fsync` on close), `SyncNone` (no sync),
  or `SyncDataSync` (`fdatasync` on Linux). Call `Sync()` explicitly any time to
  flush mid-stream.

#### Packing many transactions per block (caching dumper)

`WriteRow`/`WriteTx` emit one logical transaction per call. To pack many
independent transactions into one compressed block — the shape Tarantool's own
xlog uses (e.g. ~50 single-row autocommit txs per `ZRowMarker` block) — use
`WriteBlock` (one physical block, rows' own TSN/commit flags delimit the
transactions inside) or the higher-level `BatchWriter`, which buffers whole
transactions and flushes a block when a `MaxTxs`/`MaxBytes` threshold trips:

```go
bw := writer.NewBatchWriter(w, writer.BatchOptions{MaxTxs: 50})
for _, tx := range incoming {
    if err := bw.WriteTx(tx.Rows); err != nil { log.Fatal(err) }
}
bw.Close() // Flushes the final block.
```

`BatchWriter` encodes each transaction eagerly, so it never retains the caller's
rows — you can feed it rows straight from a `WithAliasBodies()` reader with no
cloning.

### Rewriting metadata

`tools.RewriteMeta` rewrites only the text header and byte-copies every
transaction block verbatim, so CRCs and compressed payloads are preserved
exactly. This is the safe way to fix up UUIDs or remap replica IDs in an
existing file.

```go
// Replace the instance UUID, leaving all rows untouched:
err := tools.RewriteMeta("in.xlog", "out.xlog",
    tools.ReplaceInstanceUUID(uuid.MustParse("22222222-2222-2222-2222-222222222222")))

// Remap replica IDs in the vclock (old → new):
err = tools.RewriteMeta("in.xlog", "out.xlog",
    tools.RemapVClock(map[uint32]uint32{1: 5}))

// Or supply an arbitrary transform:
err = tools.RewriteMeta("in.xlog", "out.xlog", func(m *format.Meta) *format.Meta {
    m.PrevVClock = format.VClock{1: 100}
    return m
})
```

## Performance

The library is built for high-throughput reading, repacking, and dumping:

- **Zero-allocation row cursor** — `Scan`/`ScanTx` decode into reader-owned
  scratch; a steady-state loop allocates nothing per row. `WithAliasBodies`
  removes the per-row body copy entirely.
- **Zero-copy in-memory readers** — `OpenMmap` (memory-mapped) and
  `NewReaderBytes` slice transaction blocks straight out of the backing buffer,
  eliminating the per-block copy and read syscalls; uncompressed bodies alias
  the buffer end-to-end.
- **Hardware CRC32C** — checksums use the CPU's Castagnoli instructions via the
  standard library's accelerated path.
- **Batched compression** — `BatchWriter` packs many transactions into one zstd
  block; the level/threshold are tunable via `WithCompression`.
- **Zero-clone pipeline** — value-semantics `XRow` plus eager-encoding
  `BatchWriter` let rows flow from a `WithAliasBodies` reader to the writer
  without any cloning.

## Package reference

### Package `format`

The byte-level codec, with no I/O or filesystem concerns. Defines the core data
types — `Meta`, `XRow`, `VClock`, `Filetype` — and the encode/decode functions
(`EncodeMeta`/`DecodeMeta`, `EncodeXRow`/`DecodeXRow`,
`EncodeTxBlock`/`DecodeTxBlock`), the magic markers
(`RowMarker`/`ZRowMarker`/`EOFMarker`), layout/version constants
(`FixheaderSize`, `CompressThreshold`, `ZstdLevel`, `FormatVersion`), the
`VyKey*` vinyl-log body keys, and `TypeName`. Request types and IPROTO
header/body keys and flags come from `github.com/tarantool/go-iproto`
(`iproto.IPROTO_*`). Use it directly if you need to work below the cursor level.

### Package `reader`

Single-file, forward-only cursor.
`Open`/`OpenFS`/`NewReader`/`OpenMmap`/`NewReaderBytes` construct it; `Meta`,
`Next`, `NextTx`, `Rows`, `Txs`, and `Close` drive it.
`Scan`/`ScanTx`/`Row`/`Tx`/`Err`/`Recycle` are the zero-allocation cursor;
`NextBlockRaw` reads physical blocks verbatim.

### Package `writer`

Single-file, write-once cursor that produces an atomically-finalized file.
`Create`/`NewWriter` construct it; `WriteRow`, `CommitTx`, `WriteTx`, `Sync`,
`Close`, and `Discard` drive it. `WriteBlock` and `BatchWriter` pack many
transactions per block; `WriteRawBlock` writes a pre-framed block verbatim.

### Package `filter`

Composable, read-only row predicates for use with `pipe.Copy`.

### Package `tools`

Meta-only rewrites (`RewriteMeta`, `ReplaceInstanceUUID`, `RemapVClock`) that
preserve payload bytes and CRCs.

Full API docs:
[pkg.go.dev/github.com/tarantool/go-xlog](https://pkg.go.dev/github.com/tarantool/go-xlog).

## Format support

- **Versions:** writes `0.13`; reads `0.13` by default and `0.12` with
  `AcceptV012()`.
- **File types:** `XLOG`, `SNAP`, `VYLOG`, `RUN`, `INDEX`.
- **Integrity:** CRC32C (Castagnoli) per transaction block, validated on read,
  computed on write.
- **Compression:** zstd for transaction payloads over 2 KiB; transparent on both
  read and write.
- **Compatibility:** validated against a frozen corpus of real files from
  Tarantool 2.11–3.8 (see [`testdata/README.md`](testdata/README.md)).

## Status

Pre-1.0. The API may still shift before a tagged release. The on-disk format
handling is the most thoroughly exercised part of the library; the higher-level
directory and rotation helpers are newer.

## License

[BSD 2-Clause](LICENSE).
