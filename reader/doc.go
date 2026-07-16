// Package reader implements a single-file, forward-only cursor over
// Tarantool xlog / snap / vylog (and any other file using the same
// envelope: RUN, INDEX). It is built on top of the pure-byte `format`
// package and adds the I/O concerns the format package deliberately
// stays out of.
//
// The cursor surfaces three views over the same byte stream:
//
//   - Row-level: (*Reader).Next / (*Reader).Rows — one format.XRow at a time;
//     (*Reader).Scan / (*Reader).Row for the zero-alloc cursor.
//   - Logical-tx-level: (*Reader).NextTx / (*Reader).Txs — rows grouped by
//     tsn / commit semantics into *Transaction values; (*Reader).ScanTx
//     / (*Reader).Tx for the zero-alloc cursor.
//   - Block-level: (*Reader).NextBlockRaw — verbatim on-disk bytes per tx
//     block, for the copy/truncate fast path paired with writer.WriteRawBlock
//     (must not be interleaved with the row/tx cursors).
//
// Strictness defaults to strict: CRC mismatch, unknown magic mid-stream,
// and missing EOF marker all raise unless the caller opts in to leniency
// via SkipCorruptTx / IgnoreMissingEOF.
package reader
