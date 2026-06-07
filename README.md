# go-xlog

[![CI](https://github.com/tarantool/go-xlog/actions/workflows/ci.yml/badge.svg)](https://github.com/tarantool/go-xlog/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tarantool/go-xlog.svg)](https://pkg.go.dev/github.com/tarantool/go-xlog)
[![Go Report Card](https://goreportcard.com/badge/github.com/tarantool/go-xlog)](https://goreportcard.com/report/github.com/tarantool/go-xlog)
[![License: BSD-2-Clause](https://img.shields.io/badge/License-BSD%202--Clause-blue.svg)](LICENSE)

A pure-Go library for reading, writing, and rewriting
[Tarantool](https://www.tarantool.io/) journal files — write-ahead logs
(`.xlog`), snapshots (`.snap`), and vinyl logs (`.vylog`) — without linking the
Tarantool C runtime.

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

Requires Go 1.24+.

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
| [`filter`](#package-filter) | Composable row predicates (`And`/`Or`/`Not`, by replica, type, LSN range). |

## Performance

The library is built for high-throughput reading, repacking, and dumping:

- **Hardware CRC32C** — checksums use the CPU's Castagnoli instructions via the
  standard library's accelerated path.

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

### Package `filter`

Composable, read-only row predicates for use with `pipe.Copy`.

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
