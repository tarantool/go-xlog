package reader

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"iter"

	"github.com/tarantool/go-xlog/format"
)

// Transaction is one *logical* tx — a maximal run of consecutive xrow
// records sharing the same tsn, terminated by IsCommit. The
// reader assembles these from NextTx by reading rows until one returns
// true from IsCommit. Single-statement txs land here as 1-row
// Transactions (the format decoder infers IsCommit from absent KeyTSN).
//
// StartLSN is the LSN of the first row, useful as a stable identifier
// for the tx within the file.
type Transaction struct {
	Rows     []format.XRow
	StartLSN int64
}

// Next returns the next xrow in the stream, advancing across tx-block
// boundaries transparently. Behaviour at EOF:
//
//   - After reading the 4-byte EOFMarker, Next returns (nil, io.EOF) on
//     every subsequent call and never reads further.
//   - If the underlying read returns io.EOF before the EOFMarker is
//     observed, Next returns (nil, ErrTruncated). With IgnoreMissingEOF
//     this is downgraded to (nil, io.EOF) and eofSeen is set.
//
// On a corrupt tx (CRC mismatch / unknown magic) the cursor returns
// ErrCorruptCRC / ErrUnknownMagic; with SkipCorruptTx it instead scans
// forward for the next valid magic and resumes.
func (r *Reader) Next() (format.XRow, error) {
	if r.eofSeen {
		return format.XRow{}, io.EOF
	}

	for r.plainOff >= len(r.plain) {
		err := r.loadNextTx()
		if err != nil {
			return format.XRow{}, err
		}

		if r.eofSeen {
			return format.XRow{}, io.EOF
		}
	}

	var row format.XRow

	n, err := format.DecodeXRowInto(r.plain[r.plainOff:], &row)
	if err != nil {
		return format.XRow{}, fmt.Errorf("reader: decode row: %w", err)
	}

	if n == 0 {
		return format.XRow{}, fmt.Errorf("reader: row at offset %d: %w", r.plainOff, ErrZeroLengthDecode)
	}

	r.plainOff += n

	return row, nil
}

// Scan advances the zero-allocation cursor to the next row, returning false at
// end-of-stream or on error (check Err). It is the alloc-light counterpart to
// Next: the row decodes into Reader-owned scratch instead of forcing per-row
// heap traffic, so a steady-state loop allocates nothing per row.
//
// Retention: the row returned by Row is a value the caller owns, so the struct
// is always safe to keep. By default its BodyRaw is copied into a Reader body
// arena and stays valid until you call Recycle (call it at a batch boundary to
// reclaim that memory). With the WithAliasBodies option BodyRaw instead aliases
// the read buffer and is only valid until the next Scan — for max-throughput
// streaming consumers that do not retain bodies.
//
//	for r.Scan() {
//	    row := r.Row()
//	    // ... use / retain row ...
//	}
//	if err := r.Err(); err != nil { ... }
func (r *Reader) Scan() bool {
	return r.scanRow(&r.cur)
}

// Row returns the row decoded by the most recent successful Scan. It is only
// meaningful after Scan returned true. See Scan for the retention contract.
func (r *Reader) Row() format.XRow { return r.cur }

// Err returns the terminal error that stopped Scan / ScanTx, or nil if they
// stopped at a clean end-of-stream.
func (r *Reader) Err() error { return r.scanErr }

// Recycle resets the Scan/ScanTx body arena to empty (keeping its capacity),
// reclaiming the memory backing the BodyRaw of every row returned since the
// last Recycle. Those bodies become invalid; do not touch them afterwards (the
// row structs themselves are caller-owned value copies and stay valid).
// Streaming or batch consumers call Recycle at their batch boundary; consumers
// that accumulate rows simply never call it.
func (r *Reader) Recycle() {
	r.bodyArena = r.bodyArena[:0]
	r.txView = r.txView[:0]
	r.cur = format.XRow{}
}

// ScanTx advances the zero-allocation cursor by one logical transaction,
// reading rows until one has IsCommit set. Returns false at clean end-of-stream
// or on error (check Err); a tx left incomplete by a truncated stream surfaces
// as ErrTruncated, matching NextTx. The rows are available via Tx and follow
// the same retention contract as Scan/Row (bodies safe to retain until Recycle).
func (r *Reader) ScanTx() bool {
	if r.eofSeen || r.scanErr != nil {
		return false
	}

	r.txView = r.txView[:0]

	for {
		r.txView = append(r.txView, format.XRow{})
		if !r.scanRow(&r.txView[len(r.txView)-1]) {
			// Drop the slot we failed to fill.
			r.txView = r.txView[:len(r.txView)-1]

			if r.scanErr == nil && len(r.txView) > 0 {
				// Rows accumulated but the stream ended before IsCommit —
				// a torn tx is never well-formed (cf. NextTx).
				r.scanErr = ErrTruncated
			}

			return false
		}

		if r.txView[len(r.txView)-1].IsCommit() {
			return true
		}
	}
}

// Tx returns the rows of the transaction read by the most recent successful
// ScanTx. The slice is reused by the next ScanTx; copy it if you need the slice
// to survive (the row bodies stay valid until Recycle).
func (r *Reader) Tx() []format.XRow { return r.txView }

// scanRow is the shared decode step behind Scan and ScanTx. It loads the next
// tx block as needed, decodes one row into dst, and (unless WithAliasBodies)
// copies the body into the body arena so the row is safe to retain. Returns
// false at end-of-stream or on error, with scanErr set for the latter.
func (r *Reader) scanRow(dst *format.XRow) bool {
	if r.eofSeen || r.scanErr != nil {
		return false
	}

	for r.plainOff >= len(r.plain) {
		if err := r.loadNextTx(); err != nil {
			r.scanErr = err

			return false
		}

		if r.eofSeen {
			return false
		}
	}

	n, err := format.DecodeXRowInto(r.plain[r.plainOff:], dst)
	if err != nil {
		r.scanErr = fmt.Errorf("reader: decode row: %w", err)

		return false
	}

	if n == 0 {
		r.scanErr = fmt.Errorf("reader: row at offset %d: %w", r.plainOff, ErrZeroLengthDecode)

		return false
	}

	if !r.cfg.aliasBodies && len(dst.BodyRaw) > 0 {
		start := len(r.bodyArena)
		r.bodyArena = append(r.bodyArena, dst.BodyRaw...)
		dst.BodyRaw = r.bodyArena[start:len(r.bodyArena)]
	}

	r.plainOff += n

	return true
}

// NextTx returns the next logical transaction by reading rows
// until one of them has IsCommit set. A 1-row single-statement tx
// returns as a Transaction with len(Rows)==1, because format.DecodeXRow
// infers IsCommit from absent KeyTSN.
//
// At end-of-stream (EOF marker observed with no rows pending) returns
// (nil, io.EOF). If the underlying read truncates mid-tx (rows have
// been accumulated but EOF appears before IsCommit), returns
// ErrTruncated regardless of IgnoreMissingEOF — a half-tx is never a
// well-formed log.
func (r *Reader) NextTx() (*Transaction, error) {
	var rows []format.XRow

	for {
		row, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(rows) == 0 {
					return nil, io.EOF
				}
				// Rows accumulated but stream ended without IsCommit —
				// the tx is incomplete on disk. Surface as truncation
				// independently of IgnoreMissingEOF: that option only
				// covers the missing 4-byte trailer, not a torn tx.
				return nil, ErrTruncated
			}

			return nil, err
		}

		rows = append(rows, row)
		if row.IsCommit() {
			return &Transaction{Rows: rows, StartLSN: rows[0].LSN}, nil
		}
	}
}

// Rows returns a Go 1.23 iterator over (format.XRow, error) pairs. The
// iterator stops at io.EOF without yielding it. Any other error is
// yielded once and then the iterator stops. The same Reader can also be
// driven via Next directly — Rows is sugar, not a separate state
// machine.
func (r *Reader) Rows() iter.Seq2[format.XRow, error] {
	return func(yield func(format.XRow, error) bool) {
		for {
			row, err := r.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}

				yield(format.XRow{}, err)

				return
			}

			if !yield(row, nil) {
				return
			}
		}
	}
}

// Txs returns a Go 1.23 iterator over (*Transaction, error) pairs. Same
// stop semantics as Rows.
func (r *Reader) Txs() iter.Seq2[*Transaction, error] {
	return func(yield func(*Transaction, error) bool) {
		for {
			tx, err := r.NextTx()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}

				yield(nil, err)

				return
			}

			if !yield(tx, nil) {
				return
			}
		}
	}
}

// NextBlockRaw reads the next physical tx block verbatim — its full on-disk
// bytes (fixheader + payload) — into a reusable Reader-owned buffer, verifies
// the CRC32C, and returns the slice without decoding any rows. It is the read
// half of the verbatim block-copy fast path (pair with writer.WriteRawBlock):
// a copy/truncate that does not transform rows can forward blocks byte-for-byte,
// skipping row decode, re-encode, recompression, and the destination's second
// CRC. A compressed (ZRow) block stays compressed — the bytes are exactly what
// is on disk.
//
// The returned slice aliases a Reader-owned buffer and is valid only until the
// next NextBlockRaw call. EOF handling mirrors Next: after the 4-byte EOFMarker
// it returns (nil, io.EOF) on every subsequent call; an unexpected EOF before
// the marker is ErrTruncated (or clean io.EOF with IgnoreMissingEOF). A corrupt
// block surfaces ErrCorruptCRC / ErrUnknownMagic, or — with SkipCorruptTx —
// resyncs forward to the next valid magic.
//
// NextBlockRaw must not be interleaved with the row cursors (Next / Scan /
// ScanTx) on the same Reader; pick one read mode per Reader.
func (r *Reader) NextBlockRaw() ([]byte, error) {
	if r.eofSeen {
		return nil, io.EOF
	}

	if r.br == nil {
		return r.nextBlockRawBytes()
	}

	for {
		magic, err := r.peekMagic()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// HandleMissingEOF either sets eofSeen and returns nil (clean
				// EOF with IgnoreMissingEOF) or returns ErrTruncated.
				if e := r.handleMissingEOF(); e != nil {
					return nil, e
				}

				return nil, io.EOF
			}

			return nil, fmt.Errorf("reader: peek magic: %w", err)
		}

		switch magic {
		case format.EOFMarker:
			if _, err := r.br.Discard(format.MarkerSize); err != nil {
				return nil, fmt.Errorf("reader: discard EOF marker: %w", err)
			}

			r.eofSeen = true
			r.markerSeen = true

			return nil, io.EOF
		case format.RowMarker, format.ZRowMarker:
			block, err := r.readBlockRaw()
			if err != nil {
				if errors.Is(err, errResync) {
					continue
				}

				return nil, err
			}

			return block, nil
		default:
			if r.cfg.skipCorruptTx {
				if err := r.scanForwardToMagic(); err != nil {
					return nil, err
				}

				continue
			}

			// Copy into a branch-local before slicing: taking magic[:] in this
			// cold path would otherwise force the [4]byte onto the heap on
			// every (hot-path) call via escape analysis.
			bad := magic

			return nil, fmt.Errorf("%w: %x", ErrUnknownMagic, bad[:])
		}
	}
}

// loadNextTx reads one tx block (or the EOF marker) from r.br and
// populates r.rowsLeft. On EOF marker it sets r.eofSeen=true and returns
// nil. On clean io.EOF before the EOF marker it returns ErrTruncated
// (or, with IgnoreMissingEOF, sets eofSeen=true and returns nil).
//
// With SkipCorruptTx, CRC mismatches and unknown magic trigger a
// forward scan for the next valid magic; we then recurse into
// loadNextTx from the new position.
func (r *Reader) loadNextTx() error {
	if r.br == nil {
		return r.loadNextTxBytes()
	}
	// We are here because the previous block (if any) is fully drained — so
	// advance the resume offset past it before loading the next one.
	r.consumed += r.blockBytes
	r.blockBytes = 0

	// SkipCorruptTx recovery must iterate, not recurse: a file of many small
	// corrupt blocks would otherwise overflow the stack (one frame per skipped
	// block), an uncatchable fatal crash on untrusted input. This is the same
	// for/errResync/continue structure as NextBlockRaw.
	for {
		// Peek the 4-byte magic without consuming, so we can handle the EOF
		// marker (which is only 4 bytes total, no fixheader/payload) without
		// having to put bytes back.
		magic, err := r.peekMagic()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return r.handleMissingEOF()
			}

			return fmt.Errorf("reader: peek magic: %w", err)
		}

		switch magic {
		case format.EOFMarker:
			// Consume the marker so subsequent reads see clean EOF.
			if _, err := r.br.Discard(format.MarkerSize); err != nil {
				return fmt.Errorf("reader: discard EOF marker: %w", err)
			}

			r.eofSeen = true
			r.markerSeen = true

			return nil
		case format.RowMarker, format.ZRowMarker:
			err := r.readTxBlock()
			if err != nil {
				// A corrupt block under SkipCorruptTx already resynced
				// forward; loop to read from the new position.
				if errors.Is(err, errResync) {
					continue
				}

				return err
			}

			return nil
		default:
			if r.cfg.skipCorruptTx {
				if err := r.scanForwardToMagic(); err != nil {
					return err
				}

				continue
			}

			// Copy into a branch-local before slicing: taking magic[:] in this
			// cold path would otherwise force the [4]byte onto the heap on every
			// (hot-path) call via escape analysis.
			bad := magic

			return fmt.Errorf("%w: %x", ErrUnknownMagic, bad[:])
		}
	}
}

// peekMagic returns the next 4 bytes without consuming them. Returns
// io.EOF iff the underlying reader is at clean EOF (no partial bytes
// buffered). A partial read mid-magic surfaces as io.ErrUnexpectedEOF
// wrapped in ErrTruncated by the caller.
func (r *Reader) peekMagic() ([4]byte, error) {
	b, err := r.br.Peek(format.MarkerSize)
	if err != nil {
		// Bufio.Reader.Peek returns io.EOF if it cannot deliver n bytes
		// AND the buffer is empty; otherwise it returns
		// io.ErrUnexpectedEOF along with the partial bytes (n>0).
		if errors.Is(err, io.EOF) && len(b) == 0 {
			return [4]byte{}, io.EOF
		}

		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// Partial bytes — file ended mid-magic. That is always a
			// truncation, the EOF marker is exactly 4 bytes.
			return [4]byte{}, fmt.Errorf("%w: %d partial bytes before EOF marker", ErrTruncated, len(b))
		}

		return [4]byte{}, fmt.Errorf("reader: peek magic: %w", err)
	}

	var m [4]byte
	copy(m[:], b)

	return m, nil
}

// readTxBlock reads one fixheader + payload from r.br, validates the
// CRC, decompresses if needed, splits into row slices, and stores them
// in r.rowsLeft. Magic must already be verified to be RowMarker or
// ZRowMarker.
func (r *Reader) readTxBlock() error {
	// Read the full 19-byte fixheader into the Reader-owned buffer (a stack
	// array here would escape into io.ReadFull and allocate per block).
	if _, err := io.ReadFull(r.br, r.fhBuf[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: short fixheader", ErrTruncated)
		}

		return fmt.Errorf("reader: read fixheader: %w", err)
	}

	if err := format.DecodeFixheaderInto(r.fhBuf, &r.fh); err != nil {
		// PeekMagic already ruled out EOFMarker and unknown magic for
		// this path, so any error here is a shape problem with the
		// padding/uints inside the fixheader. Treat as fatal.
		return fmt.Errorf("reader: decode fixheader: %w", err)
	}

	header := &r.fh

	// Guard against a corrupt/hostile length prefix forcing a huge
	// allocation before any payload byte is read.
	if header.Len > MaxTxPayloadLen {
		return fmt.Errorf("%w: %d > %d", ErrTxTooLarge, header.Len, MaxTxPayloadLen)
	}

	// Read the on-disk payload.
	if cap(r.txBuf) < int(header.Len) {
		r.txBuf = make([]byte, header.Len)
	} else {
		r.txBuf = r.txBuf[:header.Len]
	}

	if _, err := io.ReadFull(r.br, r.txBuf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: short tx payload (want %d bytes)", ErrTruncated, header.Len)
		}

		return fmt.Errorf("reader: read tx payload: %w", err)
	}

	// CRC over the on-disk bytes (pre-decompression for ZRow).
	if got := format.CRC32C(r.txBuf); got != header.CRC32C {
		if r.cfg.skipCorruptTx {
			// Don't try to seek back: the bad bytes are already gone
			// from the buffer. Resume scanning from the current
			// position — Tarantool's xlog_cursor_find_tx_magic does the
			// same. Return errResync so loadNextTx's loop retries
			// iteratively rather than recursing (stack-overflow DoS).
			if err := r.scanForwardToMagic(); err != nil {
				return err
			}

			return errResync
		}

		return fmt.Errorf("%w: have 0x%08x, want 0x%08x", ErrCorruptCRC, got, header.CRC32C)
	}

	// Decompress (or alias) into plainBuf.
	var plain []byte

	if header.Magic == format.ZRowMarker {
		out, err := format.DecompressTx(r.txBuf, r.plainBuf)
		if err != nil {
			return fmt.Errorf("reader: decompress tx: %w", err)
		}

		r.plainBuf = out
		plain = out
	} else {
		plain = r.txBuf
	}

	// Stash the plain payload and reset the row cursor. Rows are decoded
	// lazily off plainOff during Next/Scan — no pre-split [][]byte, no
	// throwaway per-row decode (that double-decode was the reader's biggest
	// per-row allocation source).
	r.plain = plain
	r.plainOff = 0
	// Record this block's on-disk size; the resume offset advances past it on
	// the next load, once its rows are drained (see loadNextTx). Set only on
	// the clean path — the SkipCorruptTx resync returns earlier.
	r.blockBytes = int64(format.FixheaderSize) + int64(header.Len)

	return nil
}

// readBlockRaw reads one fixheader + payload into r.rawBuf and verifies the
// CRC32C, *without* decoding any rows. Magic must already be verified to be
// RowMarker or ZRowMarker. It returns the contiguous fixheader+payload slice
// (aliasing r.rawBuf, valid until the next call). On a CRC mismatch under
// SkipCorruptTx it resyncs forward and returns errResync to signal the
// NextBlockRaw loop to retry.
func (r *Reader) readBlockRaw() ([]byte, error) {
	// Read the 19-byte fixheader into the Reader-owned buffer (a stack array
	// here would escape into io.ReadFull and allocate per block).
	if _, err := io.ReadFull(r.br, r.fhBuf[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: short fixheader", ErrTruncated)
		}

		return nil, fmt.Errorf("reader: read fixheader: %w", err)
	}

	if err := format.DecodeFixheaderInto(r.fhBuf, &r.fh); err != nil {
		return nil, fmt.Errorf("reader: decode fixheader: %w", err)
	}

	header := &r.fh

	// Guard against a corrupt/hostile length prefix forcing a huge allocation
	// before any payload byte is read.
	if header.Len > MaxTxPayloadLen {
		return nil, fmt.Errorf("%w: %d > %d", ErrTxTooLarge, header.Len, MaxTxPayloadLen)
	}

	// RawBuf holds the contiguous fixheader+payload for verbatim forwarding.
	// It grows to the largest block seen and is reused thereafter, so a stream
	// of similar-sized blocks stops allocating once it reaches the high-water
	// mark (cf. TxBuf in readTxBlock).
	total := format.FixheaderSize + int(header.Len)
	if cap(r.rawBuf) < total {
		r.rawBuf = make([]byte, total)
	} else {
		r.rawBuf = r.rawBuf[:total]
	}

	copy(r.rawBuf[:format.FixheaderSize], r.fhBuf[:])

	payload := r.rawBuf[format.FixheaderSize:total]
	if _, err := io.ReadFull(r.br, payload); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: short tx payload (want %d bytes)", ErrTruncated, header.Len)
		}

		return nil, fmt.Errorf("reader: read tx payload: %w", err)
	}

	// CRC over the on-disk bytes (pre-decompression for ZRow).
	if got := format.CRC32C(payload); got != header.CRC32C {
		if r.cfg.skipCorruptTx {
			// The bad bytes are already gone from the buffer; resume scanning
			// from the current position (cf. ReadTxBlock / Tarantool's
			// xlog_cursor_find_tx_magic).
			if err := r.scanForwardToMagic(); err != nil {
				return nil, err
			}

			return nil, errResync
		}

		return nil, fmt.Errorf("%w: have 0x%08x, want 0x%08x", ErrCorruptCRC, got, header.CRC32C)
	}

	// A whole block was consumed cleanly; advance the resume boundary past it.
	r.consumed += int64(total)

	return r.rawBuf, nil
}

// scanForwardToMagic advances r.br one byte at a time until a 4-byte
// peek matches RowMarker, ZRowMarker, or EOFMarker. Mirrors
// xlog_cursor_find_tx_magic (src/box/xlog.c:1989).
//
// Returns io.EOF wrapped as ErrTruncated (or io.EOF with
// IgnoreMissingEOF) if the underlying reader hits EOF first — by then
// we already know we were in a corrupt-tx recovery path so there is no
// valid EOF marker possible.
func (r *Reader) scanForwardToMagic() error {
	for {
		b, err := r.br.Peek(format.MarkerSize)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Out of data while scanning — treat as missing EOF
				// marker, honour IgnoreMissingEOF.
				return r.handleMissingEOF()
			}

			if errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("%w: %d partial bytes during resync", ErrTruncated, len(b))
			}

			return fmt.Errorf("reader: scan peek: %w", err)
		}

		if bytes.Equal(b, format.RowMarker[:]) ||
			bytes.Equal(b, format.ZRowMarker[:]) ||
			bytes.Equal(b, format.EOFMarker[:]) {
			return nil
		}
		// Discard 1 byte and keep scanning.
		if _, err := r.br.Discard(1); err != nil {
			return fmt.Errorf("reader: scan discard: %w", err)
		}
	}
}

// handleMissingEOF is the single point where the reader decides between
// surfacing ErrTruncated and downgrading to clean io.EOF. Called from
// every path that detects an unexpected EOF on the underlying reader.
func (r *Reader) handleMissingEOF() error {
	if r.cfg.ignoreMissingEOF {
		r.eofSeen = true

		return nil
	}

	return ErrTruncated
}

// --- in-memory (NewReaderBytes / OpenMmap) cursor ---
//
// These mirror loadNextTx / readTxBlock / NextBlockRaw / scanForwardToMagic but
// slice the next block directly out of r.buf instead of copying it through a
// bufio buffer. The row-decoding machinery (r.plain / r.plainOff / the body
// arena) is shared; only the step that produces the next plain payload differs.
// They are selected by the r.br == nil dispatch in loadNextTx / NextBlockRaw.

// loadNextTxBytes is the in-memory analogue of loadNextTx. For an uncompressed
// (RowMarker) block r.plain aliases r.buf, so row decoding — and, with
// WithAliasBodies, the row bodies themselves — are zero-copy.
func (r *Reader) loadNextTxBytes() error {
	// The previous block (if any) is fully drained — advance the resume offset
	// past it before loading the next one (drain-time, mirrors loadNextTx).
	r.consumed += r.blockBytes
	r.blockBytes = 0

	// SkipCorruptTx recovery must iterate, not recurse (see loadNextTx): a file
	// of many small corrupt blocks would otherwise overflow the stack.
	for {
		if len(r.buf)-r.pos < format.MarkerSize {
			// Fewer than 4 bytes remain. At an exact boundary that is a missing
			// EOF marker; a partial magic is always a truncation.
			if r.pos == len(r.buf) {
				return r.handleMissingEOF()
			}

			return fmt.Errorf("%w: %d partial bytes before EOF marker", ErrTruncated, len(r.buf)-r.pos)
		}

		var magic [4]byte

		copy(magic[:], r.buf[r.pos:r.pos+format.MarkerSize])

		switch magic {
		case format.EOFMarker:
			r.pos += format.MarkerSize
			r.eofSeen = true
			r.markerSeen = true

			return nil
		case format.RowMarker, format.ZRowMarker:
			err := r.readTxBlockBytes()
			if err != nil {
				// A corrupt block under SkipCorruptTx already resynced
				// forward; loop to read from the new position.
				if errors.Is(err, errResync) {
					continue
				}

				return err
			}

			return nil
		default:
			if r.cfg.skipCorruptTx {
				if err := r.scanForwardToMagicBytes(); err != nil {
					return err
				}

				continue
			}

			bad := magic

			return fmt.Errorf("%w: %x", ErrUnknownMagic, bad[:])
		}
	}
}

// readTxBlockBytes slices one fixheader + payload out of r.buf, validates the
// CRC, and stashes the plain payload (aliasing r.buf for RowMarker, decompressed
// into plainBuf for ZRow). Magic must already be verified RowMarker/ZRowMarker.
func (r *Reader) readTxBlockBytes() error {
	if len(r.buf)-r.pos < format.FixheaderSize {
		return fmt.Errorf("%w: short fixheader", ErrTruncated)
	}

	copy(r.fhBuf[:], r.buf[r.pos:r.pos+format.FixheaderSize])

	if err := format.DecodeFixheaderInto(r.fhBuf, &r.fh); err != nil {
		return fmt.Errorf("reader: decode fixheader: %w", err)
	}

	header := &r.fh

	if header.Len > MaxTxPayloadLen {
		return fmt.Errorf("%w: %d > %d", ErrTxTooLarge, header.Len, MaxTxPayloadLen)
	}

	payloadStart := r.pos + format.FixheaderSize

	payloadEnd := payloadStart + int(header.Len)
	if payloadEnd > len(r.buf) {
		return fmt.Errorf("%w: short tx payload (want %d bytes)", ErrTruncated, header.Len)
	}

	payload := r.buf[payloadStart:payloadEnd]

	if got := format.CRC32C(payload); got != header.CRC32C {
		if r.cfg.skipCorruptTx {
			// Resume scanning from just past the block we mis-read, matching the
			// streaming reader (its bytes are already consumed by that point).
			// Return errResync so loadNextTxBytes's loop retries iteratively
			// rather than recursing (stack-overflow DoS).
			r.pos = payloadEnd
			if err := r.scanForwardToMagicBytes(); err != nil {
				return err
			}

			return errResync
		}

		return fmt.Errorf("%w: have 0x%08x, want 0x%08x", ErrCorruptCRC, got, header.CRC32C)
	}

	if header.Magic == format.ZRowMarker {
		out, err := format.DecompressTx(payload, r.plainBuf)
		if err != nil {
			return fmt.Errorf("reader: decompress tx: %w", err)
		}

		r.plainBuf = out
		r.plain = out
	} else {
		r.plain = payload // zero-copy: aliases r.buf
	}

	r.plainOff = 0
	r.pos = payloadEnd
	// Record this block's on-disk size; consumed advances past it on the next
	// load once its rows are drained (see loadNextTxBytes).
	r.blockBytes = int64(format.FixheaderSize) + int64(header.Len)

	return nil
}

// nextBlockRawBytes is the in-memory analogue of NextBlockRaw: it returns the
// next physical block as a slice directly into r.buf (no rawBuf copy), still
// CRC-verified.
func (r *Reader) nextBlockRawBytes() ([]byte, error) {
	for {
		if len(r.buf)-r.pos < format.MarkerSize {
			if r.pos == len(r.buf) {
				if e := r.handleMissingEOF(); e != nil {
					return nil, e
				}

				return nil, io.EOF
			}

			return nil, fmt.Errorf("%w: %d partial bytes before EOF marker", ErrTruncated, len(r.buf)-r.pos)
		}

		var magic [4]byte

		copy(magic[:], r.buf[r.pos:r.pos+format.MarkerSize])

		switch magic {
		case format.EOFMarker:
			r.pos += format.MarkerSize
			r.eofSeen = true
			r.markerSeen = true

			return nil, io.EOF
		case format.RowMarker, format.ZRowMarker:
			block, err := r.readBlockRawBytes()
			if err != nil {
				if errors.Is(err, errResync) {
					continue
				}

				return nil, err
			}

			return block, nil
		default:
			if r.cfg.skipCorruptTx {
				if err := r.scanForwardToMagicBytes(); err != nil {
					return nil, err
				}

				continue
			}

			bad := magic

			return nil, fmt.Errorf("%w: %x", ErrUnknownMagic, bad[:])
		}
	}
}

// readBlockRawBytes returns the contiguous fixheader+payload slice into r.buf
// for one block after CRC-verifying it. On a CRC mismatch under SkipCorruptTx it
// resyncs and returns errResync (cf. ReadBlockRaw).
func (r *Reader) readBlockRawBytes() ([]byte, error) {
	if len(r.buf)-r.pos < format.FixheaderSize {
		return nil, fmt.Errorf("%w: short fixheader", ErrTruncated)
	}

	copy(r.fhBuf[:], r.buf[r.pos:r.pos+format.FixheaderSize])

	if err := format.DecodeFixheaderInto(r.fhBuf, &r.fh); err != nil {
		return nil, fmt.Errorf("reader: decode fixheader: %w", err)
	}

	header := &r.fh

	if header.Len > MaxTxPayloadLen {
		return nil, fmt.Errorf("%w: %d > %d", ErrTxTooLarge, header.Len, MaxTxPayloadLen)
	}

	total := format.FixheaderSize + int(header.Len)
	if r.pos+total > len(r.buf) {
		return nil, fmt.Errorf("%w: short tx payload (want %d bytes)", ErrTruncated, header.Len)
	}

	payload := r.buf[r.pos+format.FixheaderSize : r.pos+total]

	if got := format.CRC32C(payload); got != header.CRC32C {
		if r.cfg.skipCorruptTx {
			r.pos += total
			if err := r.scanForwardToMagicBytes(); err != nil {
				return nil, err
			}

			return nil, errResync
		}

		return nil, fmt.Errorf("%w: have 0x%08x, want 0x%08x", ErrCorruptCRC, got, header.CRC32C)
	}

	block := r.buf[r.pos : r.pos+total] // Zero-copy slice into r.buf.
	r.pos += total
	// Raw cursor: the whole block is consumed on return, so advance the resume
	// offset directly (no drain phase, cf. readBlockRaw).
	r.consumed += int64(total)

	return block, nil
}

// scanForwardToMagicBytes advances r.pos one byte at a time until a 4-byte
// window matches a known magic, the in-memory analogue of scanForwardToMagic.
func (r *Reader) scanForwardToMagicBytes() error {
	for {
		if len(r.buf)-r.pos < format.MarkerSize {
			return r.handleMissingEOF()
		}

		m := r.buf[r.pos : r.pos+format.MarkerSize]
		if bytes.Equal(m, format.RowMarker[:]) ||
			bytes.Equal(m, format.ZRowMarker[:]) ||
			bytes.Equal(m, format.EOFMarker[:]) {
			return nil
		}

		r.pos++
	}
}
