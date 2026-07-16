package reader

import "github.com/tarantool/go-xlog/format"

// readerCfg captures the resolved configuration for a single Reader. It is
// per-instance and not shared across readers.
type readerCfg struct {
	// SkipCorruptTx mirrors xlog_cursor_find_tx_magic
	// (src/box/xlog.c:1989): on CRC mismatch or unknown magic mid-stream,
	// scan forward byte-by-byte for the next RowMarker/ZRowMarker and
	// resume there. Bytes between the corruption and the next magic are
	// dropped.
	skipCorruptTx bool
	// IgnoreMissingEOF turns the absence of the 4-byte EOFMarker
	// from an ErrTruncated into a clean io.EOF. Useful when reading an
	// xlog the writer has not yet finished.
	ignoreMissingEOF bool
	// MetaOpts is passed through to format.DecodeMeta. AcceptV012 is the
	// only opt-in lever; everything else stays strict by default.
	metaOpts format.MetaOptions
	// AliasBodies makes the Scan/ScanTx cursor leave BodyRaw aliasing the
	// read buffer instead of copying it into the body arena. Faster (no
	// per-row body copy) but the row is only valid until the next Scan — for
	// streaming consumers that do not retain rows.
	aliasBodies bool
}

// Option configures a Reader at construction time. Options are
// applied in order; the Reader is otherwise immutable.
type Option func(*readerCfg)

// SkipCorruptTx enables resync after a CRC mismatch or unknown-magic byte
// mid-stream: the cursor scans forward byte-by-byte for the next valid
// RowMarker / ZRowMarker / EOFMarker and resumes there. Mirrors
// `xlog_cursor_find_tx_magic` (src/box/xlog.c:1989).
//
// Without this option the cursor surfaces ErrCorruptCRC / ErrUnknownMagic
// at the moment of failure.
func SkipCorruptTx() Option {
	return func(c *readerCfg) { c.skipCorruptTx = true }
}

// IgnoreMissingEOF downgrades a missing EOF marker from
// ErrTruncated to clean io.EOF. Intended for use cases where the file is
// being read while still being written.
func IgnoreMissingEOF() Option {
	return func(c *readerCfg) { c.ignoreMissingEOF = true }
}

// AcceptV012 propagates to format.MetaOptions.AcceptV012, letting the
// reader open files with format version "0.12" in addition to "0.13".
func AcceptV012() Option {
	return func(c *readerCfg) { c.metaOpts.AcceptV012 = true }
}

// WithAliasBodies makes the Scan / ScanTx cursor skip the per-row body copy:
// Row().BodyRaw aliases the reader's internal read buffer and is only valid
// until the next Scan / ScanTx (and is clobbered when the next tx block loads).
// Use it for max-throughput streaming consumers that fully process each row
// before advancing and never retain rows past the next Scan. Without it the
// default Scan contract holds — rows are safe to retain until Recycle. It has
// no effect on Next / NextTx.
func WithAliasBodies() Option {
	return func(c *readerCfg) { c.aliasBodies = true }
}
