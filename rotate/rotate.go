package rotate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/writer"
)

// ErrClosed is returned by any RotatingWriter method called after Close.
var ErrClosed = errors.New("rotate: closed")

// Sentinel errors returned by RotatingWriter methods.
var (
	// ErrEmptyRows is returned by WriteTx when given a zero-length rows slice.
	ErrEmptyRows = errors.New("rotate: WriteTx: empty rows")
	// ErrNoActiveWriter is returned when a rotation is attempted with no
	// active inner writer.
	ErrNoActiveWriter = errors.New("rotate: rotate without an active writer")
)

// rowHeaderEstimateBytes is the per-row constant added to the running
// curBytes estimate alongside len(row.BodyRaw). We do not have a direct
// Writer.BytesWritten() API; the estimate covers the xrow header map plus
// the fixheader's share. A fixed +30 per row is a pragmatic over-estimate
// (header map is ≈10-25 bytes; fixheader is 19 amortised across the tx).
const rowHeaderEstimateBytes = 30

// dirPerm is the mode for the chain directory created by New (owner rwx,
// group rx, no world access).
const dirPerm = 0o750

// RotatingWriter wraps writer.Writer with directory awareness: it produces
// a chain of files in one directory, threading PrevVClock/VClock between
// them so the chain stays consistent. Rotation is size-based and happens
// between logical transactions only.
//
// The writer is safe for concurrent WriteTx callers — mu serialises every
// operation that touches the in-flight state.
type RotatingWriter struct {
	dirPath  string
	filetype format.Filetype
	instance uuid.UUID
	opts     rotateCfg

	mu sync.Mutex

	// Cur is the active inner writer. Nil only after Close.
	cur *writer.Writer
	// CurOpenVClock is the VClock that was written into cur's Meta.VClock
	// at open time. It becomes the PrevVClock of the next file when we
	// rotate.
	curOpenVClock format.VClock
	// CurBytes tracks the rough byte estimate of payloads written into
	// cur so far. Reset to 0 on rotate.
	curBytes int64

	// RunningVClock advances as rows are written; its current value is
	// what we stamp into the next file's Meta.VClock at rotation time.
	runningVClock format.VClock

	closed bool
}

// New creates a new RotatingWriter rooted at dirPath. The directory is
// created (with MkdirAll, mode 0750) if it does not already exist.
//
// StartVClock seeds both the running vclock and the first file's
// Meta.VClock. The first file's Meta.PrevVClock is empty (this is the
// head of the chain).
//
// Instance is the InstanceUUID stamped into every file's meta.
func New(
	dirPath string,
	filetype format.Filetype,
	instance uuid.UUID,
	startVClock format.VClock,
	opts ...Option,
) (*RotatingWriter, error) {
	cfg := defaultRotateCfg()
	for _, opt := range opts {
		opt(&cfg)
	}

	err := os.MkdirAll(dirPath, dirPerm)
	if err != nil {
		return nil, fmt.Errorf("rotate: mkdir %q: %w", dirPath, err)
	}

	// Validate filetype up front via the extension lookup; opening the
	// first writer would surface this later, but a precise error from New
	// is friendlier.
	if _, err := filetype.Ext(); err != nil {
		return nil, fmt.Errorf("rotate: new: %w", err)
	}

	// Normalise nil → empty VClock. VClock.Clone() preserves nil, which would
	// leave runningVClock a nil map and panic on the first WriteTx LSN-advance
	// (assignment to a nil map). An empty VClock behaves identically to nil for
	// signature/comparison purposes but is writable.
	running := startVClock.Clone()
	if running == nil {
		running = format.VClock{}
	}

	rw := &RotatingWriter{
		dirPath:       dirPath,
		filetype:      filetype,
		instance:      instance,
		opts:          cfg,
		runningVClock: running,
		curOpenVClock: running.Clone(),
	}

	// Open the head-of-chain file: PrevVClock empty (nil), VClock = start.
	err = rw.openCurrent(nil)
	if err != nil {
		return nil, err
	}

	return rw, nil
}

// WriteTx writes rows as a single logical transaction. If the running
// byte estimate is at or above MaxFileSize on entry, the writer rotates
// first (so the new tx lands in a fresh file). The tx is never split:
// once we begin writing rows, we finish them in the current file.
func (rw *RotatingWriter) WriteTx(rows []format.XRow) error {
	if len(rows) == 0 {
		return ErrEmptyRows
	}

	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return ErrClosed
	}

	// Rotation check: only between transactions. If we are at or
	// above the threshold we rotate now, before writing this tx.
	if rw.curBytes >= rw.opts.maxFileSize {
		err := rw.rotateLocked()
		if err != nil {
			return err
		}
	}

	// Advance runningVClock from each row's (ReplicaID, LSN) before the
	// write. We accept the rows verbatim; LSN ordering is the caller's
	// responsibility.
	for _, r := range rows {
		if r.LSN > rw.runningVClock[r.ReplicaID] {
			rw.runningVClock[r.ReplicaID] = r.LSN
		}
	}

	err := rw.cur.WriteTx(rows)
	if err != nil {
		return fmt.Errorf("rotate: WriteTx: %w", err)
	}

	// Update the byte estimate: per-row header overhead + payload bytes.
	for _, r := range rows {
		rw.curBytes += int64(rowHeaderEstimateBytes + len(r.BodyRaw))
	}

	return nil
}

// Close flushes and finalises the current writer (atomic-renames its
// .inprogress to its final name via writer.Writer.Close) and marks
// the RotatingWriter closed. Subsequent calls return ErrClosed.
func (rw *RotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return ErrClosed
	}

	rw.closed = true
	if rw.cur == nil {
		return nil
	}

	err := rw.cur.Close()
	rw.cur = nil

	if err != nil {
		return fmt.Errorf("rotate: close current: %w", err)
	}

	return nil
}

// openCurrent opens a new inner writer at <dirPath>/<sig>.<ext>.inprogress
// with Meta.VClock = runningVClock and the caller-provided PrevVClock.
// RunningVClock is snapshotted into curOpenVClock so a subsequent rotate
// can use the exact opened-VClock as the next file's PrevVClock.
//
// Caller must hold rw.mu.
func (rw *RotatingWriter) openCurrent(prevVClock format.VClock) error {
	ext, err := rw.filetype.Ext()
	if err != nil {
		return fmt.Errorf("rotate: open: %w", err)
	}

	sig := rw.runningVClock.Signature()
	name := fmt.Sprintf("%020d%s", sig, ext)
	path := filepath.Join(rw.dirPath, name)

	// Snapshot the vclock at open time. The new file's Meta.VClock equals
	// the running vclock at this moment; the *next* rotation's
	// PrevVClock will be this same snapshot.
	openVClock := rw.runningVClock.Clone()

	meta := &format.Meta{
		Filetype:     rw.filetype,
		Version:      "go-xlog/rotate",
		InstanceUUID: rw.instance,
		VClock:       openVClock.Clone(),
		PrevVClock:   prevVClock,
	}

	w, err := writer.Create(path, meta, rw.opts.writerOpts...)
	if err != nil {
		return fmt.Errorf("rotate: create %q: %w", path, err)
	}

	rw.cur = w
	rw.curOpenVClock = openVClock
	rw.curBytes = 0

	return nil
}

// rotateLocked closes the active writer and opens a fresh one. The new
// file's PrevVClock equals the just-closed file's VClock-at-open:
// every tx written into the closed file advanced runningVClock, so by
// the time we open the next file runningVClock has moved on and becomes
// that file's VClock.
//
// Caller must hold rw.mu.
func (rw *RotatingWriter) rotateLocked() error {
	if rw.cur == nil {
		return ErrNoActiveWriter
	}

	err := rw.cur.Close()
	if err != nil {
		// Leave rw.cur non-nil so the caller can attempt Close again;
		// surface the error for diagnosis.
		return fmt.Errorf("rotate: close prior file: %w", err)
	}

	prev := rw.curOpenVClock // The just-closed file's opened VClock.
	rw.cur = nil

	return rw.openCurrent(prev.Clone())
}
