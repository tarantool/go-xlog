# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)
and this project adheres to [Semantic
Versioning](http://semver.org/spec/v2.0.0.html) except to the first release.

## [Unreleased]

### Added

- format: `XRow.Sync` and `XRow.SchemaVersion`, the wire-only header keys
  `IPROTO_SYNC` and `IPROTO_SCHEMA_VERSION`. `DecodeXRowInto` decodes them and
  `EncodeXRow` emits them iff non-zero, `IPROTO_SYNC` where Tarantool's
  `xrow_header_encode` writes it (right after the request type) and
  `IPROTO_SCHEMA_VERSION` right after it.
- format: `VyLogType`, the type of a vinyl log record, with a constant and a
  name for every record type of Tarantool.
- format: Typed decoders for the rows of vinyl `.run` and `.index` files:
  `DecodeVyStmtBody` for the INSERT/REPLACE/DELETE/UPSERT statements of a run
  (with their `VyStmtFlags`: deferred DELETE, skip read),
  `DecodeVyRowIndexBody` for the row index that ends each page,
  `DecodeVyRunInfoBody` and `DecodeVyPageInfoBody` for the run and page infos
  of an index file. `TypeName` names their row types (`VY_INDEX_RUN_INFO`,
  `VY_INDEX_PAGE_INFO`, `VY_RUN_ROW_INDEX`) `RUNINFO`, `PAGEINFO` and
  `ROWINDEX`, as Tarantool does.
- format: `VClockArray`, a dense vclock of `VClockMax` (32, Tarantool's
  `VCLOCK_MAX`) components that copies by value and allocates nothing, with
  `Get`, `Set`, `Has`, `Len`, `Signature`, `Compare` and `String` agreeing
  with `VClock`, `DecodeVClockArrayInto` for its msgpack map and the
  converters `VClockArray.VClock` and `VClockArrayFrom`. A replica id
  `>= VClockMax` fails with `ErrVClockReplicaID`. `VClock` is unchanged.
- format: `DecodeOps` and the allocation-free `DecodeOpsInto` decode the
  operations of an UPDATE (its `DMLBody.Tuple`) or an UPSERT (its
  `DMLBody.Ops`) into `[]Operation`: the opcode, the field by number or by
  name, the splice position and offset, and the value as raw msgpack
  aliasing the body.

### Changed

- format: `XRow` has new fields. A composite literal of `XRow` without field
  names no longer compiles, and a row decoded from bytes that carry the new
  keys no longer compares equal (`==`, `assert.Equal`, as a map key) to a
  hand-built row that leaves them out.
- format: `VyLogBody.Type` is a `VyLogType` instead of a `uint32`.
- dir: `OpenDir` and `OpenDirFS` refuse the `RUN` and `INDEX` filetypes with
  `ErrNotChain`: vinyl run files are named by run id and carry no vclock, so
  they failed with a misleading `ErrSignatureMismatch`.

### Fixed

- format: `DecodeSynchroBody` reads the term of a PROMOTE/DEMOTE row from
  `IPROTO_TERM`, where Tarantool writes it, instead of key 0, which left the
  term zero. The confirmed vclock a PROMOTE/DEMOTE carries (`IPROTO_VCLOCK`,
  written by the limbo checkpoint of a snapshot) is decoded into the new
  `SynchroBody.ConfirmedVClock`.
- format: `DecodeVyLogBody` decodes the record of a VYLOG row from the
  `[type, {key: value}]` tuple under `IPROTO_TUPLE`, the shape Tarantool's
  `vy_log_record_encode` writes. It used to read the body map itself as the
  record, so every record decoded as type 0 (`CREATE_LSM`) with the whole
  tuple as its only key; a body without the tuple is now an error
  (`ErrBadVyLogRecord`).

## [v0.1.0] - 2026-07-23

First release of `go-xlog`, a pure-Go byte-level codec and toolkit for the
Tarantool journal format (XLOG/SNAP/VYLOG/RUN/INDEX). It provides
format-faithful encode/decode (Tarantool 3.x `0.13`, legacy `0.12` read
support), single-file and directory-aware readers/writers with atomic durable
finalization, composable row filters, streaming pipes, and live tailing
(`follow`) with restart checkpointing. Iteration is built on `iter.Seq2`
range-over-func cursors with zero-allocation and zero-copy fast paths.

### Added

- format: Pure byte-level codec for the Tarantool journal format — `Meta`,
  `XRow`, `VClock`, and encode/decode helpers with no I/O. Format-faithful to
  Tarantool 3.x (version `0.13`), reads legacy `0.12` with `AcceptV012()`,
  and mirrors the C implementation byte-for-byte (CRC32C/Castagnoli, msgpack
  row encoding, zstd compression threshold). File types `XLOG`, `SNAP`,
  `VYLOG`, `RUN`, and `INDEX`.
- filter: Composable row predicates (`And`/`Or`/`Not`, by replica, type, and
  LSN range).
- reader: Single-file forward cursor with row- and transaction-level
  iteration over `iter.Seq2` range-over-func iterators. Zero-allocation
  `Scan`/`ScanTx` cursor (with `WithAliasBodies` to drop the per-row body
  copy), zero-copy `OpenMmap` and `NewReaderBytes` in-memory readers, and a
  verbatim `NextBlockRaw` cursor.
- reader: `OpenAt` (resume reading at a prior block-boundary offset), `Offset`
  (the current resume offset), and `SawEOFMarker` (whether the file is
  finalised).
- writer: Single-file write-once cursor with atomic finalize, a `BatchWriter`
  that packs many transactions into one zstd block, tunable compression via
  `WithCompression`, and `WriteRawBlock` for verbatim block forwarding.
- tools: Meta-only rewrites that preserve payload bytes and CRCs.
- dir: Immutable in-memory index of a journal directory; locate files by LSN
  and vclock.
- rotate: Directory-aware writer that rotates files by size and threads
  vclocks across rotations.
- pipe: Stream filtered transactions from a reader to a writer, including the
  verbatim `CopyRaw` fast path.
- follow: Live tailing of journals. `follow.File` blocks for rows appended to a
  single file and ends when it is finalised; `follow.Dir` follows a rotation
  chain, switching to the next file on finalisation and validating chain
  continuity. Both are `iter.Seq2` iterators (with `FileTx`/`DirTx` variants)
  cancellable via `context.Context`, with a pluggable `Watcher` (default
  polling, no new dependency), optional `WithReadInprogress` low-latency
  tailing of the active `.inprogress` file, and a `Follower` type exposing
  `Offset()` for restart checkpointing. Backed by `reader.OpenAt`,
  `reader.Offset`, and `reader.SawEOFMarker`.

### Fixed

- writer/rotate/tools: fsync the parent directory after the `.inprogress` →
  final atomic rename. Previously only the file contents were synced, so a
  power loss could leave a "committed" segment stranded as `.inprogress` or
  missing despite `Close`/`RewriteMeta` returning success. Affects every
  `writer.Writer.Close`, every rotation, and `tools.RewriteMeta` (skipped
  under `SyncNone`, which opts out of Close-time durability). New
  `internal/durable.SyncDir` helper.
