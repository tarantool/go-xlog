# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)
and this project adheres to [Semantic
Versioning](http://semver.org/spec/v2.0.0.html) except to the first release.

## [Unreleased]

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

### Changed

### Fixed

- writer/rotate/tools: fsync the parent directory after the `.inprogress` →
  final atomic rename. Previously only the file contents were synced, so a
  power loss could leave a "committed" segment stranded as `.inprogress` or
  missing despite `Close`/`RewriteMeta` returning success. Affects every
  `writer.Writer.Close`, every rotation, and `tools.RewriteMeta` (skipped
  under `SyncNone`, which opts out of Close-time durability). New
  `internal/durable.SyncDir` helper.
