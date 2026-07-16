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
