// Package rotate implements a directory-aware writer that produces a chain
// of journal files inside a single directory. Rotation is size-based: when
// the current file's running byte estimate crosses MaxFileSize, the writer
// closes the current file (Writer.Close atomically renames .inprogress →
// final name) and opens a new one whose Meta.PrevVClock equals the
// just-closed file's Meta.VClock.
//
// Rotation happens *between* logical transactions, never mid-tx.
// All rows of a logical tx live in one file — Tarantool itself flushes the
// journal queue at wal_opt_rotate (src/box/wal.c) and we mirror that here
// by only checking the size threshold on entry to WriteTx.
//
// The writer holds all state on its receiver; there is no package-level
// mutable state. Filesystem concerns (os.MkdirAll, file naming) live in
// this layer, not in `format`.
package rotate

import (
	"github.com/tarantool/go-xlog/writer"
)

// defaultMaxFileSize is the size at which the writer rotates by default.
// 64 MiB matches a reasonable WAL chunk and keeps directory scans fast.
const defaultMaxFileSize int64 = 64 * 1024 * 1024

// rotateCfg captures the resolved per-instance configuration for a
// RotatingWriter — per-instance, no package-level mutable state.
type rotateCfg struct {
	// MaxFileSize is the rotation threshold in bytes. We use an estimated
	// running counter (fixheader + payload) and trip the rotation on the
	// next WriteTx entry once the counter ≥ maxFileSize.
	maxFileSize int64

	// WriterOpts is the slice of WriterOptions to pass through to every
	// inner writer.Writer constructed by the rotating writer. Composable:
	// successive WriterOptions(...) Calls append, they do not replace.
	writerOpts []writer.Option
}

// defaultRotateCfg returns the baseline configuration before option
// application.
func defaultRotateCfg() rotateCfg {
	return rotateCfg{
		maxFileSize: defaultMaxFileSize,
	}
}

// Option configures a RotatingWriter at construction time. Options
// are applied in order; the RotatingWriter is otherwise immutable.
type Option func(*rotateCfg)

// MaxFileSize sets the per-file rotation threshold in bytes. Default is
// 64 MiB. The threshold is checked between transactions only — a single
// large transaction may exceed it.
//
// Values ≤ 0 are accepted (no validation at option time) — they will
// effectively rotate on every WriteTx since the running estimate is
// non-negative.
func MaxFileSize(n int64) Option {
	return func(c *rotateCfg) { c.maxFileSize = n }
}

// WriterOptions appends WriterOptions to propagate to every inner Writer
// constructed by the rotating writer. Useful for forwarding NoCompression,
// Sync, Version, etc. Composable across multiple calls.
func WriterOptions(opts ...writer.Option) Option {
	return func(c *rotateCfg) {
		c.writerOpts = append(c.writerOpts, opts...)
	}
}
