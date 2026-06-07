// Package pipe streams logical transactions between a reader and a writer,
// optionally applying row-level filters with any-row-matches semantics.
//
// Pipelines are transaction-aware. Filtering happens at tx
// granularity — if any row inside a logical tx passes the filter, the
// entire tx is written; otherwise the entire tx is dropped. Partial
// transactions are never emitted.
//
// Copy decodes and re-encodes every tx (the writer re-runs
// assignTxIDs, which can re-assign TSN), which is required for filtering and
// row transforms. CopyRaw is the verbatim fast path: it forwards whole physical
// blocks byte-for-byte (no decode/encode/recompression/second CRC) for the
// pure copy/truncate case where no predicate is needed.
package pipe

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/tarantool/go-xlog/filter"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// Sentinel errors returned by Copy when its arguments are nil.
var (
	// ErrNilSrcReader is returned by Copy when src is nil.
	ErrNilSrcReader = errors.New("pipe.Copy: nil src reader")
	// ErrNilDstWriter is returned by Copy when dst is nil.
	ErrNilDstWriter = errors.New("pipe.Copy: nil dst writer")
)

// Copy streams logical transactions from src to dst. For each tx in src,
// the predicate `filter.Or(fs...)` is applied to every row; if any row
// matches (or fs is empty, in which case every tx is kept), the whole tx
// is written via dst.WriteTx. Returns the total number of rows written.
//
// Errors short-circuit — the first reader or writer error is returned and
// the destination is left in whatever state it was in. The caller owns
// closing both src and dst.
func Copy(src *reader.Reader, dst *writer.Writer, fs ...filter.Filter) (int64, error) {
	if src == nil {
		return 0, ErrNilSrcReader
	}

	if dst == nil {
		return 0, ErrNilDstWriter
	}

	// KeepAll short-circuits the predicate evaluation when no filters were
	// supplied. We deliberately don't fold this into a "match-all" filter
	// because constructing filter.Or() with no args is vacuously false —
	// the *predicate* needs to be vacuously true on an empty filter list.
	keepAll := len(fs) == 0
	pred := filter.Or(fs...)

	var rowsWritten int64

	for {
		tx, err := src.NextTx()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return rowsWritten, nil
			}

			return rowsWritten, fmt.Errorf("pipe.Copy: read tx: %w", err)
		}

		keep := keepAll
		if !keep {
			if slices.ContainsFunc(tx.Rows, pred) {
				keep = true
			}
		}

		if !keep {
			continue
		}

		if err := dst.WriteTx(tx.Rows); err != nil {
			return rowsWritten, fmt.Errorf("pipe.Copy: write tx: %w", err)
		}

		rowsWritten += int64(len(tx.Rows))
	}
}

// CopyRaw streams physical tx blocks from src to dst verbatim, forwarding each
// block's full on-disk bytes (fixheader + payload) without decoding rows. It is
// the fast path for a pure copy/truncate: no row decode, no re-encode, no
// recompression, and no second CRC — a compressed source block stays compressed
// on disk. Returns the number of blocks copied.
//
// Unlike Copy it cannot filter — filtering needs row-level decoding, so use
// Copy when you need a predicate. Errors short-circuit; the caller owns closing
// both src and dst.
func CopyRaw(src *reader.Reader, dst *writer.Writer) (int64, error) {
	if src == nil {
		return 0, ErrNilSrcReader
	}

	if dst == nil {
		return 0, ErrNilDstWriter
	}

	var blocks int64

	for {
		block, err := src.NextBlockRaw()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return blocks, nil
			}

			return blocks, fmt.Errorf("pipe.CopyRaw: read block: %w", err)
		}

		if err := dst.WriteRawBlock(block); err != nil {
			return blocks, fmt.Errorf("pipe.CopyRaw: write block: %w", err)
		}

		blocks++
	}
}
