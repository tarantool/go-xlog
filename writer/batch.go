package writer

import (
	"fmt"

	"github.com/tarantool/go-xlog/format"
)

// BatchOptions configures a BatchWriter's block-flush policy. A buffered
// block is flushed when EITHER limit is reached, whichever trips first:
//
//   - MaxTxs:   flush once this many logical transactions are buffered.
//   - MaxBytes: flush once the buffered payload reaches this many bytes.
//
// A zero limit disables that dimension. With both zero the BatchWriter never
// auto-flushes — transactions accumulate into one block until Flush or Close,
// which is rarely what you want for a large stream. MaxBytes is measured as the
// encoded (pre-compression) payload size buffered so far — the exact plain
// block size — so it is a tight target for on-disk block size before zstd.
type BatchOptions struct {
	MaxTxs   int
	MaxBytes int
}

// BatchWriter packs many independent logical transactions into compressed tx
// blocks. Each WriteTx call is one logical transaction: BatchWriter assigns
// its TSN/commit flags (via assignTxIDs) and appends its rows to a flat
// buffer. When a BatchOptions threshold is reached the buffer is flushed as
// one physical block via the writer's internal block framing — so each buffered transaction
// keeps its own identity (single-row txs stay autocommit) while still sharing
// a zstd-compressed block. This is the "caching dumper" shape: the grouping
// Tarantool's xlog uses, which plain WriteTx cannot produce because it merges
// its rows into one logical tx.
//
// Not safe for concurrent use. The BatchWriter takes ownership of the
// underlying Writer for its lifetime; do not call the Writer's WriteRow /
// CommitTx / WriteTx / WriteBlock directly while a BatchWriter wraps it.
//
// WriteTx encodes each transaction into the pending byte buffer immediately and
// never retains the caller's rows, so rows fed straight from a reader's
// recycled decode buffers (including aliased BodyRaw) need no cloning.
type BatchWriter struct {
	w   *Writer
	opt BatchOptions

	pending []byte // Accumulated plain tx payload; tx boundaries live in row flags.
	txCount int    // Logical transactions buffered since last flush.
}

// NewBatchWriter returns a BatchWriter that flushes blocks to w according to
// opt. It does not take any action on its own; call WriteTx to feed
// transactions and Close (or Flush) to emit the final block.
func NewBatchWriter(w *Writer, opt BatchOptions) *BatchWriter {
	return &BatchWriter{w: w, opt: opt}
}

// WriteTx buffers one logical transaction (rows sharing a tx). AssignTxIDs is
// applied to rows (shared TSN = first row's LSN, only the last row commits),
// mutating them in place, then the rows are *encoded* straight into the pending
// payload buffer — the caller's rows are not retained, so they may be reused or
// recycled immediately. When a flush threshold is reached the buffer is written
// as a single (possibly compressed) block. Returns ErrEmptyRows if rows is
// empty.
func (b *BatchWriter) WriteTx(rows []format.XRow) error {
	if len(rows) == 0 {
		return ErrEmptyRows
	}

	assignTxIDs(rows) // Stamp this tx's boundaries into the row flags.

	var err error

	b.pending, err = format.AppendTxBlockPayload(b.pending, rows)
	if err != nil {
		return fmt.Errorf("writer: batch encode tx: %w", err)
	}

	b.txCount++

	if b.shouldFlush() {
		return b.Flush()
	}

	return nil
}

// Flush writes any buffered transactions as one block and resets the buffer.
// It is a no-op when nothing is buffered. On a write error the buffer is left
// intact so the caller may retry or Discard.
func (b *BatchWriter) Flush() error {
	if len(b.pending) == 0 {
		return nil
	}

	if err := b.w.writeBlockPayload(b.pending); err != nil {
		return err
	}

	b.pending = b.pending[:0]
	b.txCount = 0

	return nil
}

// Close flushes any buffered transactions, then closes the underlying Writer
// (writing the EOF marker and, for a file writer, renaming into place). If
// the final flush fails the Writer is left open so the caller can Discard.
func (b *BatchWriter) Close() error {
	if err := b.Flush(); err != nil {
		return err
	}

	return b.w.Close()
}

// shouldFlush reports whether either configured threshold has been reached.
func (b *BatchWriter) shouldFlush() bool {
	if b.opt.MaxTxs > 0 && b.txCount >= b.opt.MaxTxs {
		return true
	}

	if b.opt.MaxBytes > 0 && len(b.pending) >= b.opt.MaxBytes {
		return true
	}

	return false
}
