// Package writer implements a single-file, write-once cursor that produces
// Tarantool xlog / snap / vylog / run / index files in the durable journal
// format. It is built on top of the pure-byte `format` package and adds the
// I/O concerns the format package deliberately stays out of.
//
// A Writer:
//   - opens `<path>.inprogress` exclusively on construction;
//   - encodes the meta header via format.EncodeMeta;
//   - accepts rows in one of two API shapes — append-then-commit
//     (WriteRow / CommitTx) or whole-tx (WriteTx);
//   - flushes per-tx via format.EncodeTxBlock (zstd-compresses payloads at or above CompressThreshold);
//   - on Close, writes the EOFMarker, flushes, fsyncs per cfg, then atomically
//     renames `<path>.inprogress` → `<path>`.
//
// Strictness defaults: O_EXCL on .inprogress (no silent overwrite),
// post-Close calls return ErrClosed, mid-tx WriteTx errors out rather than
// silently flushing.
package writer

import "github.com/tarantool/go-xlog/format"

// SyncMode controls the Close-time durability behaviour of a Writer. Only
// Close-time sync is exposed in v1 — per-write syncs are not.
type SyncMode int

const (
	// SyncNone disables Close-time fsync. The OS may delay the bytes hitting
	// the platter past Close return; only Rename atomicity remains.
	SyncNone SyncMode = iota

	// SyncNormal is the default: f.Sync() on Close before rename.
	SyncNormal

	// SyncDataSync uses syscall.Fdatasync on Linux (data-only sync; skips
	// the inode metadata flush f.Sync() implies). Falls back to f.Sync()
	// on platforms without Fdatasync.
	SyncDataSync
)

// writerCfg captures the resolved per-instance configuration for a Writer.
// Per-instance only; no package-level mutable state.
type writerCfg struct {
	// Compression is the block compression policy for every tx written by this
	// writer (zstd level + threshold, or disabled). Zero value = Tarantool's
	// default (zstd at ZstdLevel over payloads >= CompressThreshold).
	compression format.Compression

	// Version, if non-empty, populates Meta.Version when the caller-supplied
	// meta has an empty Version field at Create() time. The default
	// "go-xlog/0.1" is applied when neither the meta nor an explicit option
	// provided a value.
	version string

	// Sync chooses the Close-time durability behaviour. Default SyncNormal.
	sync SyncMode
}

// defaultCfg returns the baseline configuration before option application.
func defaultCfg() writerCfg {
	return writerCfg{
		compression: format.Compression{}, // Zero value = default zstd policy.
		version:     "",
		sync:        SyncNormal,
	}
}

// Option configures a Writer at construction time. Options are applied
// in order; the Writer is otherwise immutable.
type Option func(*writerCfg)

// WithCompression sets the block compression policy for the writer: the zstd
// level, the minimum payload size at which a block is compressed, or disabling
// compression entirely. A zero-value field takes its default (ZstdLevel /
// CompressThreshold), so format.Compression{Level: 9} tunes only the level.
func WithCompression(c format.Compression) Option {
	return func(cfg *writerCfg) { cfg.compression = c }
}

// NoCompression disables zstd compression for every tx written by this writer,
// regardless of payload size — sugar for
// WithCompression(format.Compression{Disabled: true}). Useful for test fixtures
// whose byte content needs to be inspected, and for tools whose downstream
// consumer cannot decompress.
func NoCompression() Option {
	return func(c *writerCfg) { c.compression.Disabled = true }
}

// Version sets the Meta.Version string written to the file's text header
// when the caller-supplied Meta has Version == "". Pass-through Meta.Version
// values always win — this option only fills in the blank.
func Version(s string) Option {
	return func(c *writerCfg) { c.version = s }
}

// Sync chooses the Close-time durability behaviour: SyncNone (skip), SyncNormal
// (f.Sync(), the default), or SyncDataSync (Fdatasync on Linux, fallback to
// f.Sync() elsewhere).
func Sync(mode SyncMode) Option {
	return func(c *writerCfg) { c.sync = mode }
}
