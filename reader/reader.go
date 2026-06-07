package reader

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/tarantool/go-xlog/format"
)

// Public sentinel errors. Mid-stream failures map to one of these; meta
// errors propagate from the format package unwrapped.
var (
	// ErrTruncated is returned when the cursor reaches an unexpected EOF
	// before observing the 4-byte EOFMarker. Suppressed to clean
	// io.EOF when IgnoreMissingEOF is set.
	ErrTruncated = errors.New("reader: file truncated before EOF marker")

	// ErrCorruptCRC is returned when a tx block's computed CRC32C does
	// not match the value in its fixheader. With SkipCorruptTx
	// the cursor resyncs instead of surfacing this error.
	ErrCorruptCRC = errors.New("reader: tx block CRC mismatch")

	// ErrUnknownMagic is returned when a 4-byte magic prefix is neither
	// RowMarker, ZRowMarker, nor EOFMarker. With SkipCorruptTx the cursor
	// scans forward for the next valid magic instead.
	ErrUnknownMagic = errors.New("reader: unknown magic bytes in stream")

	// ErrTxTooLarge is returned when a fixheader declares a tx payload
	// length above MaxTxPayloadLen — a corrupt/hostile length prefix must
	// not drive an unbounded allocation before the bytes are even read.
	ErrTxTooLarge = errors.New("reader: tx payload length exceeds MaxTxPayloadLen")

	// ErrZeroLengthDecode is returned when the row decoder consumes zero
	// bytes for a row within a tx payload — a malformed payload that would
	// otherwise spin the split loop forever.
	ErrZeroLengthDecode = errors.New("zero-length decode")
)

// errResync is an internal sentinel returned by readBlockRaw when a corrupt
// block was skipped under SkipCorruptTx and the NextBlockRaw loop should retry
// from the resynced position. It never escapes the package.
var errResync = errors.New("reader: resync")

// MaxTxPayloadLen caps the per-tx payload the reader will allocate for. The
// fixheader's length field is an untrusted uint32 (up to 4 GiB); without a
// ceiling a 19-byte hostile file claiming a huge length would force a giant
// allocation up front. 1 GiB is far above any real Tarantool tx (the
// autocommit-flush threshold is 128 KiB; the largest single tuple is bounded
// by memtx_max_tuple_size) while bounding the worst case.
const MaxTxPayloadLen = 1 << 30

// Reader is a single-file, forward-only cursor over an xlog / snap /
// vylog / run / index byte stream. Construct via Open (path-shaped, the
// Reader owns the file) or NewReader (io-shaped, the caller retains
// ownership).
//
// A Reader is not safe for concurrent use. Create one per consumer; each
// instance carries its own buffers and zstd decoder handle.
type Reader struct {
	cfg readerCfg

	// Owned is set when Open created the underlying file and Close must
	// release it; NewReader-constructed readers leave this nil and Close
	// is then a no-op for I/O.
	owned io.Closer

	// Br wraps the underlying stream. We sized it at AutocommitThreshold
	// (128 KiB) — large enough to hold one typical tx in memory and the
	// same number Tarantool's writer uses for its tx flush threshold. We
	// must keep using this same bufio.Reader after DecodeMeta because the
	// meta parser will have already read ahead into the buffer past the
	// header terminator.
	br *bufio.Reader

	// Meta is the parsed text header; immutable after construction.
	meta *format.Meta

	// Plain is the decompressed (or as-is) payload of the most recently
	// loaded tx block; plainOff is the cursor into it for the next row. The
	// reader decodes rows lazily off this offset — no per-tx [][]byte split,
	// no per-row double decode. Plain aliases txBuf (plain tx) or plainBuf
	// (zrow tx) and is clobbered on the next block load.
	plain    []byte
	plainOff int

	// EofSeen is set once we reach end-of-stream — either by reading the
	// 4-byte EOFMarker or, with IgnoreMissingEOF, by hitting a clean EOF
	// before it. After that Next() returns io.EOF and never advances the
	// stream.
	eofSeen bool

	// MarkerSeen is set only when the real 4-byte EOFMarker was read — i.e.
	// the file is finalised on disk. It is NOT set by the IgnoreMissingEOF
	// downgrade (a still-being-written file). SawEOFMarker() exposes it so a
	// follower can tell "writer finished this file" from "writer paused
	// mid-append". See handleMissingEOF, which deliberately leaves it false.
	markerSeen bool

	// Consumed is the absolute byte offset of the first byte the cursor has
	// not yet fully consumed — the start of the current in-memory block while
	// its rows are still being emitted, advancing past that block only once it
	// is drained. It is the resume point Offset() reports and OpenAt seeks to.
	// Advancing at drain time (not load time) keeps it a safe resume offset
	// even mid-block: resuming there re-reads the current block (at-least-once)
	// rather than skipping its undrained rows. blockBytes is the on-disk size
	// of the currently loaded row-cursor block, added to consumed when the next
	// load drains it. Both are maintained in the streaming and in-memory row
	// cursors alike; the raw cursor (NextBlockRaw) instead advances consumed
	// directly as each whole block is returned.
	consumed   int64
	blockBytes int64

	// ScanErr is the terminal error of the Scan/ScanTx cursor, surfaced via
	// Err(). Nil at clean EOF.
	scanErr error

	// Backing the zero-alloc Scan/ScanTx cursor. Rows are returned by
	// value, so the caller always owns its struct copy; bodyArena holds the
	// copied body bytes so a retained row's BodyRaw stays valid. BodyArena
	// grows by amortised doubling and is reset (length 0, capacity kept) by
	// Recycle. Cur is the row from the last Scan; txView is the reused []XRow
	// returned by Tx.
	bodyArena []byte
	txView    []format.XRow
	cur       format.XRow

	// Reusable per-block scratch — kept on the Reader so the tight block
	// loop allocates nothing per tx block. FhBuf backs the io.ReadFull of
	// the fixheader (a Reader field instead of a stack array that would
	// escape into Read); fh receives the decoded fixheader in place.
	fhBuf [format.FixheaderSize]byte
	fh    format.Fixheader

	// Reusable scratch buffers — these grow with the largest tx seen.
	txBuf    []byte // Payload bytes read from disk (on-disk: maybe zstd).
	plainBuf []byte // Decompressed bytes for ZRow tx; aliased by plain.
	rawBuf   []byte // fixheader+payload for the NextBlockRaw verbatim cursor.

	// Buf/pos back the in-memory cursor used by NewReaderBytes and OpenMmap:
	// the whole file is already in memory, so the block loop slices each
	// fixheader/payload directly out of buf with no txBuf copy, and an
	// uncompressed (RowMarker) block's plain payload aliases buf end-to-end
	// (valid for the Reader's lifetime — the buffer outlives it). In this mode
	// br is nil and the streaming path (br != nil) is bypassed; pos is the read
	// offset into buf. A ZRow block still decompresses into plainBuf.
	buf []byte
	pos int
}

// Open opens path for reading and parses its meta header. The returned
// Reader owns the file and releases it on Close. This is the
// path-shaped façade over the io-shaped NewReader.
func Open(path string, opts ...Option) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reader: open %q: %w", path, err)
	}

	r, err := newReader(f, opts...)
	if err != nil {
		// Best effort: surface the open-time failure but still release the
		// file we just acquired. A failure-to-close on top of a failure
		// here is reported with errors.Join so callers can inspect both.
		closeErr := f.Close()
		if closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("reader: close %q: %w", path, closeErr))
		}

		return nil, err
	}

	r.owned = f

	return r, nil
}

// OpenFS opens name within fsys and parses its meta header. It is the fs.FS
// analogue of Open: the returned Reader owns the opened fs.File and closes it
// on Close. Use it to read a journal from an embed.FS, fstest.MapFS, archive
// filesystem, or any other io/fs.FS. Name follows the io/fs path contract
// (slash-separated, unrooted).
func OpenFS(fsys fs.FS, name string, opts ...Option) (*Reader, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("reader: open %q: %w", name, err)
	}

	r, err := newReader(f, opts...)
	if err != nil {
		closeErr := f.Close()
		if closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("reader: close %q: %w", name, closeErr))
		}

		return nil, err
	}

	r.owned = f // An fs.File is an io.Closer.

	return r, nil
}

// ReadHeader opens path, parses its meta header, closes the file, and returns
// an independent copy of the parsed header. It is the one-shot convenience for
// callers that want only the header, not a row cursor — equivalent to Open +
// Meta + Close, but it reads just the header region (tx blocks are never
// touched). The returned *format.Meta is a Clone the caller may freely mutate
// (e.g. to feed a writer or tools.RewriteMetaFields).
//
// It accepts the same Options as Open (e.g. AcceptV012).
func ReadHeader(path string, opts ...Option) (*format.Meta, error) {
	r, err := Open(path, opts...)
	if err != nil {
		return nil, err
	}

	defer func() { _ = r.Close() }()

	return r.Meta().Clone(), nil
}

// ReadHeaderFS is the fs.FS analogue of ReadHeader (cf. OpenFS).
func ReadHeaderFS(fsys fs.FS, name string, opts ...Option) (*format.Meta, error) {
	r, err := OpenFS(fsys, name, opts...)
	if err != nil {
		return nil, err
	}

	defer func() { _ = r.Close() }()

	return r.Meta().Clone(), nil
}

// NewReader wraps an existing io.ReadSeeker (we keep ReadSeeker rather
// than io.Reader for API symmetry with future seek-based features; the
// current implementation only uses the Read half). It parses the meta
// header eagerly so the caller can rely on Meta() being non-nil.
//
// Ownership of rs remains with the caller; (*Reader).Close on a Reader
// constructed this way is a no-op for I/O.
func NewReader(rs io.ReadSeeker, opts ...Option) (*Reader, error) {
	return newReader(rs, opts...)
}

// NewReaderBytes builds a Reader over a complete in-memory journal image. It is
// the zero-copy alternative to NewReader for callers that already hold the whole
// file in a byte slice (preloaded, embedded, or memory-mapped): the block loop
// slices each fixheader and payload directly out of b instead of copying through
// a bufio buffer, eliminating the per-block memmove and the read syscalls. For
// uncompressed (RowMarker) blocks the decoded rows alias b directly, so with
// WithAliasBodies the row bodies are zero-copy AND safe to retain for as long as
// b is alive (b outlives the Reader). ZRow blocks still decompress.
//
// Ownership of b remains with the caller; Close is a no-op for I/O. B must not
// be mutated while the Reader is in use.
func NewReaderBytes(b []byte, opts ...Option) (*Reader, error) {
	return newReaderBytes(b, opts...)
}

// newReaderBytes is the shared construction path for the in-memory cursor
// (NewReaderBytes and OpenMmap). It parses the meta header off the front of b
// and records the exact byte offset where the tx blocks begin.
func newReaderBytes(b []byte, opts ...Option) (*Reader, error) {
	cfg := readerCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}

	// DecodeMeta needs a *bufio.Reader and reads ahead past the header
	// terminator. The bytes it actually consumed for the meta is the total it
	// pulled from src (len(b) - src.Len()) minus what it read ahead but left
	// buffered (br.Buffered()).
	src := bytes.NewReader(b)
	br := bufio.NewReader(src)

	meta, err := format.DecodeMeta(br, cfg.metaOpts)
	if err != nil {
		return nil, fmt.Errorf("reader: decode meta: %w", err)
	}

	consumed := len(b) - src.Len() - br.Buffered()

	return &Reader{
		cfg:      cfg,
		meta:     meta,
		buf:      b,
		pos:      consumed,
		consumed: int64(consumed),
	}, nil
}

// countingReader counts the bytes read through it. newReader wraps the
// streaming source in one so it can derive the meta-header size (and thus the
// first block's byte offset) after DecodeMeta has read ahead into the bufio
// buffer: bytes-consumed = bytes-pulled − bytes-still-buffered.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	// The io.Reader contract requires the error (notably io.EOF) be returned
	// verbatim; wrapping it would break bufio's sentinel checks.
	return n, err //nolint:wrapcheck // io.Reader.Read must propagate its error as-is
}

// newReader is the shared construction path. Kept private so Open and
// NewReader differ only in the ownership invariant on the underlying
// stream.
func newReader(r io.Reader, opts ...Option) (*Reader, error) {
	cfg := readerCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}

	cr := &countingReader{r: r}
	br := bufio.NewReaderSize(cr, format.AutocommitThreshold)

	meta, err := format.DecodeMeta(br, cfg.metaOpts)
	if err != nil {
		return nil, fmt.Errorf("reader: decode meta: %w", err)
	}

	return &Reader{
		cfg:  cfg,
		br:   br,
		meta: meta,
		// Bytes logically consumed for the meta header = pulled into the
		// bufio − still buffered. This is the offset of the first tx block.
		consumed: cr.n - int64(br.Buffered()),
	}, nil
}

// OpenAt opens path, parses its meta header, and positions the cursor to begin
// reading at blockOffset — a byte offset previously reported by Offset (always
// a clean tx-block boundary past the header). It is the resume primitive behind
// follow-style tailing: re-open a growing file and continue from where a prior
// pass stopped, without re-reading the blocks already consumed.
//
// blockOffset is clamped up to the end of the meta header, so 0 (or any value
// inside the header) is equivalent to Open. The returned Reader owns the file
// and releases it on Close. It accepts the same Options as Open; pair it with
// IgnoreMissingEOF when tailing a file the writer has not finalised.
func OpenAt(path string, blockOffset int64, opts ...Option) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reader: open %q: %w", path, err)
	}

	r, err := newReaderAt(f, blockOffset, opts...)
	if err != nil {
		closeErr := f.Close()
		if closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("reader: close %q: %w", path, closeErr))
		}

		return nil, err
	}

	r.owned = f

	return r, nil
}

// newReaderAt parses the meta header from the front of f, then rebuilds the
// buffered reader positioned at blockOffset so the read-ahead from meta parsing
// is discarded and the cursor resumes exactly at a block boundary.
func newReaderAt(f *os.File, blockOffset int64, opts ...Option) (*Reader, error) {
	cfg := readerCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}

	cr := &countingReader{r: f}
	br := bufio.NewReaderSize(cr, format.AutocommitThreshold)

	meta, err := format.DecodeMeta(br, cfg.metaOpts)
	if err != nil {
		return nil, fmt.Errorf("reader: decode meta: %w", err)
	}

	metaSize := cr.n - int64(br.Buffered())
	if blockOffset < metaSize {
		// Never start inside the header; clamp up to the first block boundary.
		blockOffset = metaSize
	}

	if _, err := f.Seek(blockOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("reader: seek to %d: %w", blockOffset, err)
	}

	return &Reader{
		cfg:      cfg,
		br:       bufio.NewReaderSize(f, format.AutocommitThreshold),
		meta:     meta,
		consumed: blockOffset,
	}, nil
}

// Offset returns the byte offset of the next tx block the cursor will read — a
// clean block boundary past the meta header, advanced only when a whole block
// (or the EOFMarker) has been consumed, never mid-block. Pass it to OpenAt to
// resume a later pass exactly where this one stopped. A partial trailing block
// does not move it, so re-reading from Offset never double-emits a row.
func (r *Reader) Offset() int64 { return r.consumed }

// SawEOFMarker reports whether the cursor consumed the real 4-byte EOFMarker —
// i.e. the file is finalised on disk. It stays false when end-of-stream was
// reached only via the IgnoreMissingEOF downgrade (a file still being written).
// A follower uses it to distinguish "writer finished this file" (stop / hand
// off to the next file) from "writer paused mid-append" (wait for more bytes).
func (r *Reader) SawEOFMarker() bool { return r.markerSeen }

// Meta returns the parsed meta header. The pointer is stable for the
// lifetime of the Reader; callers must not mutate the returned value.
func (r *Reader) Meta() *format.Meta { return r.meta }

// Close releases the underlying file when this Reader owns it (i.e. was
// constructed via Open). For NewReader-constructed readers Close is a
// no-op — the caller retains ownership of the stream.
func (r *Reader) Close() error {
	if r.owned == nil {
		return nil
	}

	err := r.owned.Close()
	r.owned = nil

	if err != nil {
		return fmt.Errorf("reader: close: %w", err)
	}

	return nil
}
