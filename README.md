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

## Status

Pre-1.0. The API may still shift before a tagged release. The on-disk format
handling is the most thoroughly exercised part of the library; the higher-level
directory and rotation helpers are newer.

## License

[BSD 2-Clause](LICENSE).
