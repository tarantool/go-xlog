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
