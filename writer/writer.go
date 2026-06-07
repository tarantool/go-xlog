package writer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/durable"
)

// syncDir fsyncs the directory holding the final file so the `.inprogress` →
// final rename survives power loss. It is a package var (defaulting to
// durable.SyncDir) so tests can observe that Close issues the directory sync.
var syncDir = durable.SyncDir

// Sentinel errors returned by the Writer. Their identity matters for callers
// using errors.Is.
var (
	// ErrClosed is returned by any Writer method called after Close or Discard
	// has finished.
	ErrClosed = errors.New("writer: closed")

	// ErrNilDst is returned by NewWriter when dst is nil.
	ErrNilDst = errors.New("writer: NewWriter: nil dst")

	// ErrNilMeta is returned by Create / NewWriter when meta is nil.
	ErrNilMeta = errors.New("writer: nil meta")

	// ErrPendingWriteRow is returned by WriteTx when the WriteRow accumulator
	// is non-empty; the caller must CommitTx first.
	ErrPendingWriteRow = errors.New("writer: WriteTx with pending WriteRow buffer; call CommitTx first")

	// ErrEmptyRows is returned by WriteTx when given an empty rows slice, and
	// by WriteBlock / BatchWriter.WriteTx when a transaction has no rows.
	ErrEmptyRows = errors.New("writer: WriteTx: empty rows")

	// ErrEmptyBlock is returned by WriteBlock when given an empty txs slice.
	ErrEmptyBlock = errors.New("writer: WriteBlock: empty txs")

	// ErrInvalidRawBlock is returned by WriteRawBlock when the bytes are not a
	// well-formed framed tx block: shorter than a fixheader, or not prefixed by
	// RowMarker / ZRowMarker.
	ErrInvalidRawBlock = errors.New("writer: invalid raw block")
)

// inprogressFilePerm is the mode for the .inprogress file opened by Create
// (owner read/write only — the writer owns the file).
const inprogressFilePerm = 0o600

// Writer is a single-file, write-once cursor that produces a Tarantool
// journal file in the format described by the `format` package. Not safe
// for concurrent use; create one per writing goroutine.
//
// Lifecycle:
//
//   - Create opens `<path>.inprogress` exclusively and writes the
//     meta header.
//   - WriteRow / CommitTx (or WriteTx) append tx blocks to the file.
//   - Close writes the EOFMarker, flushes, syncs per cfg, and atomically
//     renames `.inprogress` → final. After Close all methods
//     return ErrClosed.
//   - Discard is the escape hatch: closes the fd and removes the
//     `.inprogress` file without ever producing a final file. After
//     Discard all methods return ErrClosed.
type Writer struct {
	cfg writerCfg

	// Path is the final on-disk path. InprogressPath = path + ".inprogress".
	path           string
	inprogressPath string

	// F is the underlying file handle for the .inprogress file. Nil after
	// Close / Discard.
	f *os.File

	// Bw buffers writes between Close-time flush and the kernel. Sized to
	// AutocommitThreshold so a typical tx fits in one buffer copy.
	bw *bufio.Writer

	// TxRows accumulates rows submitted via WriteRow until CommitTx flushes
	// them as a single tx block. Nil for the WriteTx fast path.
	txRows []format.XRow

	// BufferedPayloadEstimate is a rough running sum of len(row.BodyRaw)
	// across txRows; we use it to decide whether to auto-flush on commit
	// boundary per the AutocommitThreshold hint. Mid-tx the buffer can
	// exceed the threshold — a logical tx must not be split.
	bufferedPayloadEstimate int

	// Reusable encode scratch: encScratch accumulates the plain tx
	// payload, comprScratch holds the zstd output, and fh/fhBuf frame the
	// fixheader in place — all reused across txs so a steady-state write
	// allocates nothing.
	encScratch   []byte
	comprScratch []byte
	fh           format.Fixheader
	fhBuf        [format.FixheaderSize]byte

	// Closed is set after Close (success) or Discard. Once set, every
	// further call returns ErrClosed.
	closed bool
}

// Create opens path.inprogress for exclusive writing, writes the meta header,
// and returns a Writer ready for tx submission.
//
// Failure modes:
//
//   - the .inprogress file already exists (O_EXCL): error wrapping the
//     underlying os.OpenFile failure (no silent overwrite).
//   - meta encoding fails: file is closed and removed; the partial
//     .inprogress is not left behind.
func Create(path string, meta *format.Meta, opts ...Option) (*Writer, error) {
	cfg, err := resolveCfg(meta, opts)
	if err != nil {
		return nil, err
	}

	inprogress := path + ".inprogress"

	f, err := os.OpenFile(inprogress, os.O_CREATE|os.O_EXCL|os.O_WRONLY, inprogressFilePerm)
	if err != nil {
		return nil, fmt.Errorf("writer: open %q: %w", inprogress, err)
	}

	bw := bufio.NewWriterSize(f, format.AutocommitThreshold)
	if err := format.EncodeMeta(bw, meta); err != nil {
		// Roll back: close fd, remove .inprogress. We intentionally do not
		// leave a partial file behind for a Create-time failure.
		_ = f.Close()
		_ = os.Remove(inprogress)

		return nil, fmt.Errorf("writer: encode meta: %w", err)
	}

	return &Writer{
		cfg:            cfg,
		path:           path,
		inprogressPath: inprogress,
		f:              f,
		bw:             bw,
	}, nil
}

// NewWriter returns a Writer that streams to an arbitrary io.Writer instead of
// a file. It writes the meta header immediately, then accepts tx submissions
// exactly like a Create'd writer. The file-only concerns do not apply: there
// is no .inprogress / atomic rename (the caller owns dst), and Close performs
// no fsync — it flushes the EOF marker and the buffered tail, nothing more.
// SyncMode and Sync() are no-ops for an in-memory writer.
//
// Useful for producing an xlog into a buffer, a network stream, or a
// compressing writer, and for fast in-memory round-trip testing.
func NewWriter(dst io.Writer, meta *format.Meta, opts ...Option) (*Writer, error) {
	if dst == nil {
		return nil, ErrNilDst
	}

	cfg, err := resolveCfg(meta, opts)
	if err != nil {
		return nil, err
	}

	bw := bufio.NewWriterSize(dst, format.AutocommitThreshold)
	if err := format.EncodeMeta(bw, meta); err != nil {
		return nil, fmt.Errorf("writer: encode meta: %w", err)
	}

	return &Writer{cfg: cfg, bw: bw}, nil // f == nil: in-memory, no file lifecycle.
}

// resolveCfg validates meta, applies options, and fills meta.Version from the
// Version() option or the "go-xlog/0.1" default. Shared by Create and NewWriter.
func resolveCfg(meta *format.Meta, opts []Option) (writerCfg, error) {
	if meta == nil {
		return writerCfg{}, ErrNilMeta
	}

	cfg := defaultCfg()
	for _, opt := range opts {
		opt(&cfg)
	}

	if meta.Version == "" {
		if cfg.version != "" {
			meta.Version = cfg.version
		} else {
			meta.Version = "go-xlog/0.1"
		}
	}

	return cfg, nil
}

// WriteRow appends r to the writer's in-memory tx accumulator. The accumulator
// is flushed as a single tx block when either:
//
//   - the caller calls CommitTx, or
//   - r.IsCommit() is true at append time AND the buffered payload estimate is
//     at or above AutocommitThreshold (a soft hint — a logical tx must not be
//     split mid-stream, so we never auto-flush on a non-commit row).
//
// To compose multi-row txs through this path, leave IPROTO_FLAG_COMMIT cleared on all
// but the last row, and either let WriteRow auto-flush on the last (if the
// caller pre-set IPROTO_FLAG_COMMIT) or call CommitTx explicitly. AssignTxIDs is
// applied during CommitTx — the caller does not need to manage TSN.
func (w *Writer) WriteRow(r format.XRow) error {
	if w.closed {
		return ErrClosed
	}

	w.txRows = append(w.txRows, r)
	w.bufferedPayloadEstimate += len(r.BodyRaw)
	// Auto-flush only on a caller-marked commit boundary: if the caller passes
	// IsCommit=true and the buffer is at or past threshold, flush now. If the
	// buffer crosses threshold mid-tx, accept it and continue — flush only on
	// commit boundary.
	if r.IsCommit() && w.bufferedPayloadEstimate >= format.AutocommitThreshold {
		return w.CommitTx()
	}

	return nil
}

// CommitTx applies assignTxIDs to the accumulated WriteRow buffer, encodes
// the rows as one tx block, writes them through the buffered file, and clears
// the accumulator. No-op when the accumulator is empty.
func (w *Writer) CommitTx() error {
	if w.closed {
		return ErrClosed
	}

	if len(w.txRows) == 0 {
		return nil
	}

	assignTxIDs(w.txRows)

	err := w.encodeAndWriteTx(w.txRows)
	if err != nil {
		return err
	}

	w.txRows = w.txRows[:0]
	w.bufferedPayloadEstimate = 0

	return nil
}

// WriteTx is the all-in-one entry point: applies assignTxIDs, encodes the
// rows as one tx block, and writes them. It must not be mixed with a pending
// WriteRow accumulator — if the WriteRow buffer is non-empty, WriteTx returns
// an error rather than silently flushing (no implicit cross-API commits).
// Callers who want both APIs at once should explicitly CommitTx first.
func (w *Writer) WriteTx(rows []format.XRow) error {
	if w.closed {
		return ErrClosed
	}

	if len(w.txRows) != 0 {
		return ErrPendingWriteRow
	}

	if len(rows) == 0 {
		return ErrEmptyRows
	}

	assignTxIDs(rows)

	return w.encodeAndWriteTx(rows)
}

// WriteBlock writes rows as a single physical tx block, *verbatim* — it does
// NOT touch TSN or commit flags. The rows' own TSN/commit flags delimit the
// logical transactions inside the block, so one block may hold many
// transactions (the shape Tarantool's own xlog uses: e.g. ~50 single-row
// autocommit txs per zstd block). The payload is zstd-compressed when it
// crosses CompressThreshold.
//
// Because it trusts the flags, the rows must already carry valid TSN/commit —
// which is exactly how rows arrive from the reader, making WriteBlock the
// efficient primitive for copy/repack/truncate. To synthesise ONE transaction
// from freshly-built rows (LSNs only), use WriteTx, which assigns the TSN and
// commit flag for you; to pack many independent transactions into blocks
// without managing flags yourself, use BatchWriter.
//
// WriteBlock must not be interleaved with a pending WriteRow accumulator: it
// returns ErrPendingWriteRow if that buffer is non-empty, ErrEmptyBlock if
// rows is empty, and ErrClosed after Close.
func (w *Writer) WriteBlock(rows []format.XRow) error {
	if w.closed {
		return ErrClosed
	}

	if len(w.txRows) != 0 {
		return ErrPendingWriteRow
	}

	if len(rows) == 0 {
		return ErrEmptyBlock
	}

	return w.encodeAndWriteTx(rows)
}

// WriteRawBlock writes a pre-framed tx block (fixheader + payload) verbatim:
// no row decode, no re-encode, no recompression, and no CRC recomputation. The
// block must be the exact on-disk bytes of a valid tx block — already carrying
// a correct fixheader and CRC32C — as produced by reader.NextBlockRaw. It is
// the write half of the verbatim block-copy fast path: a copy/truncate that
// does not transform rows forwards source blocks byte-for-byte, so a compressed
// (ZRow) block stays compressed without a decompress/recompress round trip.
//
// Guards: ErrClosed after Close, ErrPendingWriteRow if a WriteRow accumulator
// is non-empty, and ErrInvalidRawBlock if block is shorter than a fixheader or
// is not prefixed by RowMarker / ZRowMarker. The block's CRC is trusted, not
// re-verified — it was validated when the block was read; corruption introduced
// between read and write is the caller's responsibility.
func (w *Writer) WriteRawBlock(block []byte) error {
	if w.closed {
		return ErrClosed
	}

	if len(w.txRows) != 0 {
		return ErrPendingWriteRow
	}

	if len(block) < format.FixheaderSize {
		return fmt.Errorf("%w: %d bytes < fixheader %d", ErrInvalidRawBlock, len(block), format.FixheaderSize)
	}

	// Compare the leading magic without letting a stack [4]byte escape: copy
	// into a local for the array compare, but slice block (not the local) in
	// the cold error path. Passing the local's [:] to fmt.Errorf would force
	// it to the heap on every call via static escape analysis.
	var magic [4]byte

	copy(magic[:], block[:format.MarkerSize])

	if magic != format.RowMarker && magic != format.ZRowMarker {
		return fmt.Errorf("%w: leading magic %x", ErrInvalidRawBlock, block[:format.MarkerSize])
	}

	if _, err := w.bw.Write(block); err != nil {
		return fmt.Errorf("writer: write raw block: %w", err)
	}

	return nil
}

// Sync flushes the bufio buffer to the kernel and then either f.Sync() or
// fdatasync() depending on cfg.sync. SyncNone is treated as bufio flush
// only (no kernel sync). Useful between WriteTx calls for callers that
// want stronger durability than Close-time sync.
func (w *Writer) Sync() error {
	if w.closed {
		return ErrClosed
	}

	err := w.bw.Flush()
	if err != nil {
		return fmt.Errorf("writer: flush: %w", err)
	}

	return w.syncFile()
}

// Close finalises the file:
//
//  1. Flushes any pending WriteRow accumulator via CommitTx.
//  2. Writes the 4-byte EOFMarker.
//  3. Flushes the bufio buffer.
//  4. fsyncs per cfg.sync.
//  5. Closes the fd.
//  6. Atomically renames .inprogress → final.
//  7. fsyncs the parent directory so the rename is durable (unless SyncNone).
//
// After Close all further methods return ErrClosed. If any step fails before
// the rename, the .inprogress file is left in place (the caller can inspect
// it). The Writer transitions to closed in either outcome — callers must
// Discard if they want the .inprogress removed after a failed Close.
func (w *Writer) Close() error {
	if w.closed {
		return ErrClosed
	}
	// Step 1: flush any pending tx. We must do this *before* writing the EOF
	// marker; losing a pending tx on Close would be silent data loss.
	err := w.CommitTx()
	if err != nil {
		w.closed = true
		w.releaseFile() // Best-effort fd release so the caller can inspect/clean.

		return err
	}
	// Step 2: EOF marker.
	if _, err := w.bw.Write(format.EOFMarker[:]); err != nil {
		w.closed = true
		w.releaseFile()

		return fmt.Errorf("writer: write EOF marker: %w", err)
	}
	// Step 3: flush bufio.
	err = w.bw.Flush()
	if err != nil {
		w.closed = true
		w.releaseFile()

		return fmt.Errorf("writer: flush: %w", err)
	}
	// In-memory writer (NewWriter): no fd, no rename — the EOF + flush above
	// is the whole finalisation.
	if w.f == nil {
		w.closed = true

		return nil
	}
	// Step 4: fsync per cfg.
	err = w.syncFile()
	if err != nil {
		w.closed = true
		w.releaseFile()

		return err
	}
	// Step 5: close fd.
	err = w.f.Close()
	if err != nil {
		w.closed = true
		w.f = nil

		return fmt.Errorf("writer: close fd: %w", err)
	}

	w.f = nil
	// Step 6: atomic rename.
	err = os.Rename(w.inprogressPath, w.path)
	if err != nil {
		w.closed = true

		return fmt.Errorf("writer: rename %q -> %q: %w", w.inprogressPath, w.path, err)
	}
	// Step 7: fsync the parent directory so the rename is durable. fsyncing
	// the file (Step 4) flushes its contents but not the directory entry that
	// publishes the final name; without this a power loss can leave a
	// "committed" segment stranded as .inprogress or missing. Skipped under
	// SyncNone, where the caller has opted out of Close-time durability.
	if w.cfg.sync != SyncNone {
		err = syncDir(filepath.Dir(w.path))
		if err != nil {
			w.closed = true

			return fmt.Errorf("writer: sync dir: %w", err)
		}
	}

	w.closed = true

	return nil
}

// Discard releases the file handle and removes the .inprogress file without
// promoting it to its final name. Use after a caller-side error that makes
// the in-flight file unusable.
//
// After Discard all further methods return ErrClosed. Discard is a no-op
// after Close (returns ErrClosed) because Close has already removed the
// .inprogress (via rename).
func (w *Writer) Discard() error {
	if w.closed {
		return ErrClosed
	}
	// Mark closed first so concurrent or subsequent calls all return
	// ErrClosed even if Close fails mid-step.
	w.closed = true
	// In-memory writer: no .inprogress file to remove.
	if w.f == nil && w.inprogressPath == "" {
		return nil
	}

	var firstErr error

	if w.f != nil {
		err := w.f.Close()
		if err != nil {
			firstErr = fmt.Errorf("writer: discard close fd: %w", err)
		}

		w.f = nil
	}

	err := os.Remove(w.inprogressPath)
	if err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("writer: discard remove %q: %w", w.inprogressPath, err)
		}
	}

	return firstErr
}

// encodeAndWriteTx is the shared back-end. It does NOT touch the WriteRow
// accumulator — both CommitTx and WriteTx manage that themselves. The plain
// payload is encoded into the Writer's reusable encScratch, so a steady-state
// write allocates nothing.
func (w *Writer) encodeAndWriteTx(rows []format.XRow) error {
	var err error

	w.encScratch, err = format.AppendTxBlockPayload(w.encScratch[:0], rows)
	if err != nil {
		return fmt.Errorf("writer: encode tx block: %w", err)
	}

	return w.writeFramed(w.encScratch)
}

// writeBlockPayload frames and writes an already-encoded plain tx payload as
// one physical block, applying the same guards as WriteBlock. It is the
// primitive BatchWriter flushes through: the batch accumulates encoded payload
// directly, so it never retains caller rows.
func (w *Writer) writeBlockPayload(plain []byte) error {
	if w.closed {
		return ErrClosed
	}

	if len(w.txRows) != 0 {
		return ErrPendingWriteRow
	}

	if len(plain) == 0 {
		return ErrEmptyBlock
	}

	return w.writeFramed(plain)
}

// writeFramed compresses plain per the writer's Compression policy, computes
// the CRC32C over the on-disk bytes, then writes the fixheader and
// payload through the buffered writer. The compression output and framing
// header live in Writer-owned scratch, and the fixheader + payload are written
// separately (no assembled-block buffer), so this allocates nothing in steady
// state.
func (w *Writer) writeFramed(plain []byte) error {
	payload := plain
	magic := format.RowMarker

	if w.cfg.compression.Compresses(len(plain)) {
		var err error

		w.comprScratch, err = format.CompressTxInto(w.comprScratch[:0], plain, w.cfg.compression.ResolvedLevel())
		if err != nil {
			return fmt.Errorf("writer: compress tx block: %w", err)
		}

		payload = w.comprScratch
		magic = format.ZRowMarker
	}

	w.fh = format.Fixheader{
		Magic:  magic,
		Len:    uint32(len(payload)), //nolint:gosec // G115: tx payload length is a uint32 fixheader field
		CRC32C: format.CRC32C(payload),
	}
	format.EncodeFixheader(&w.fhBuf, &w.fh)

	if _, err := w.bw.Write(w.fhBuf[:]); err != nil {
		return fmt.Errorf("writer: write fixheader: %w", err)
	}

	if _, err := w.bw.Write(payload); err != nil {
		return fmt.Errorf("writer: write tx payload: %w", err)
	}

	return nil
}

// syncFile is the cfg-aware sync. Caller has already flushed bufio. No-op for
// an in-memory writer (no fd to sync).
func (w *Writer) syncFile() error {
	if w.f == nil {
		return nil
	}

	switch w.cfg.sync {
	case SyncNone:
		return nil
	case SyncDataSync:
		err := fdatasync(w.f)
		if err != nil {
			return fmt.Errorf("writer: fdatasync: %w", err)
		}

		return nil
	case SyncNormal:
		fallthrough
	default:
		err := w.f.Sync()
		if err != nil {
			return fmt.Errorf("writer: fsync: %w", err)
		}

		return nil
	}
}

// releaseFile closes the fd if present (best-effort; for Close error paths and
// a no-op for an in-memory writer).
func (w *Writer) releaseFile() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
}
