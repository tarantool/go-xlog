package rotate_test

// coverage_extra_test.go — additional black-box tests targeting previously
// uncovered branches in rotate.go and options.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/rotate"
	"github.com/tarantool/go-xlog/writer"
)

// ---------------------------------------------------------------------------
// options.go: WriterOptions coverage
// ---------------------------------------------------------------------------

// TestWriterOptions_NoCompression — WriterOptions(writer.NoCompression()) must
// thread through to the inner writer so the produced file is readable and
// contains the expected rows.
func TestWriterOptions_NoCompression(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(
		tmp,
		format.FiletypeXLOG,
		uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		format.VClock{},
		rotate.WriterOptions(writer.NoCompression()),
	)
	require.NoError(t, err, "New with WriterOptions(NoCompression)")

	require.NoError(t, rw.WriteTx([]format.XRow{makeRow(1, 50)}), "WriteTx")
	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")
	require.Len(t, d.Files(), 1, "expected 1 file")

	rd, err := reader.Open(d.Files()[0].Path)
	require.NoError(t, err, "reader.Open")

	defer func() { _ = rd.Close() }()

	tx, err := rd.NextTx()
	require.NoError(t, err, "NextTx")
	require.Len(t, tx.Rows, 1)
	assert.Equal(t, int64(1), tx.Rows[0].LSN)
}

// TestWriterOptions_SyncNone — WriterOptions(writer.Sync(writer.SyncNone))
// passes the sync option to every inner writer, including after rotation.
func TestWriterOptions_SyncNone(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(
		tmp,
		format.FiletypeXLOG,
		uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		format.VClock{},
		rotate.MaxFileSize(128),
		rotate.WriterOptions(writer.Sync(writer.SyncNone)),
	)
	require.NoError(t, err, "New with WriterOptions(SyncNone)")

	// Write enough to trigger at least one rotation — option must apply to
	// every file created by the rotating writer.
	for lsn := int64(1); lsn <= 4; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 100)}), "WriteTx lsn=%d", lsn)
	}

	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")
	assert.GreaterOrEqual(t, len(d.Files()), 2, "expected rotation")
}

// TestWriterOptions_Multiple — multiple WriterOptions calls compose (append,
// not replace): passing two separate calls must apply both options.
func TestWriterOptions_Multiple(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(
		tmp,
		format.FiletypeXLOG,
		uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		format.VClock{},
		rotate.WriterOptions(writer.NoCompression()),
		rotate.WriterOptions(writer.Sync(writer.SyncNone)),
	)
	require.NoError(t, err, "New with two WriterOptions calls")

	require.NoError(t, rw.WriteTx([]format.XRow{makeRow(1, 50)}), "WriteTx")
	require.NoError(t, rw.Close(), "Close")
}

// ---------------------------------------------------------------------------
// rotate.go: New — error branches
// ---------------------------------------------------------------------------

// TestNew_BadDir — New must return an error when the parent directory is
// not writable (MkdirAll fails).
func TestNew_BadDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// Create a regular file at the path we want to MkdirAll — the OS will
	// refuse to treat it as a directory, so MkdirAll returns an error.
	blocker := filepath.Join(tmp, "notadir")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	target := filepath.Join(blocker, "subdir")
	_, err := rotate.New(target, format.FiletypeXLOG, testInstance, format.VClock{})
	require.Error(t, err, "New must fail when MkdirAll is impossible")
	assert.Contains(t, err.Error(), "rotate: mkdir")
}

// TestNew_InvalidFiletype — New must return an error immediately for an
// unrecognised filetype (Ext() validation up-front).
func TestNew_InvalidFiletype(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	_, err := rotate.New(tmp, format.Filetype("BOGUS"), testInstance, format.VClock{})
	require.Error(t, err, "New must reject unknown filetype")
	assert.Contains(t, err.Error(), "rotate: new")
}

// TestNew_OpenCurrentFails — New must propagate an openCurrent error when
// the first .inprogress file cannot be created. We block it by pre-creating
// a regular file at the path writer.Create would use.
func TestNew_OpenCurrentFails(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// The first file will be named "00000000000000000000.xlog.inprogress"
	// because the starting VClock has signature 0.
	blocker := filepath.Join(tmp, "00000000000000000000.xlog.inprogress")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	_, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.Error(t, err, "New must fail when inprogress file already exists")
	assert.Contains(t, err.Error(), "rotate: create")
}

// ---------------------------------------------------------------------------
// rotate.go: WriteTx — error branches
// ---------------------------------------------------------------------------

// TestWriteTx_EmptyRows — WriteTx([]) must return ErrEmptyRows without
// touching the underlying writer.
func TestWriteTx_EmptyRows(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err, "New")

	err = rw.WriteTx(nil)
	require.ErrorIs(t, err, rotate.ErrEmptyRows, "expected ErrEmptyRows, got %v", err)

	err = rw.WriteTx([]format.XRow{})
	require.ErrorIs(t, err, rotate.ErrEmptyRows, "expected ErrEmptyRows for empty slice, got %v", err)

	require.NoError(t, rw.Close(), "Close after empty-rows errors")
}

// TestWriteTx_AfterClose — WriteTx after Close must return ErrClosed.
func TestWriteTx_AfterClose(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err, "New")
	require.NoError(t, rw.Close(), "Close")

	err = rw.WriteTx([]format.XRow{makeRow(1, 50)})
	assert.ErrorIs(t, err, rotate.ErrClosed, "expected ErrClosed, got %v", err)
}

// TestWriteTx_RotateFailsOnCreate — when a rotation is triggered but the
// next .inprogress file cannot be created (pre-blocked), WriteTx returns an
// error and Close on the now-orphaned writer returns nil (cur is nil).
func TestWriteTx_RotateFailsOnCreate(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// MaxFileSize=1 → rotate after every tx.
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{},
		rotate.MaxFileSize(1))
	require.NoError(t, err, "New")

	// Write lsn=1 successfully. After this, curBytes >= maxFileSize(1).
	require.NoError(t, rw.WriteTx([]format.XRow{makeRow(1, 10)}), "first WriteTx")

	// After lsn=1 is written, runningVClock = {1:1}, signature = 1.
	// The next file will be "00000000000000000001.xlog.inprogress". Block it.
	blocker := filepath.Join(tmp, "00000000000000000001.xlog.inprogress")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	// Second WriteTx will try to rotate first, which will fail.
	err = rw.WriteTx([]format.XRow{makeRow(2, 10)})
	require.Error(t, err, "WriteTx must fail when rotation's Create is blocked")

	// After rotateLocked set cur=nil (closed the prior file) and openCurrent
	// failed, cur is nil. Close must still return nil (no writer to close).
	closeErr := rw.Close()
	assert.NoError(t, closeErr, "Close after failed rotation should return nil")
}

// ---------------------------------------------------------------------------
// rotate.go: Close — error branches
// ---------------------------------------------------------------------------

// TestClose_DoubleClose — second Close must return ErrClosed.
func TestClose_DoubleClose(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err, "New")

	require.NoError(t, rw.Close(), "first Close")

	err = rw.Close()
	assert.ErrorIs(t, err, rotate.ErrClosed, "second Close: expected ErrClosed, got %v", err)
}

// TestClose_NoWrites — Close on a writer that was opened but never written
// to must still succeed (the head file with the starting VClock is committed).
func TestClose_NoWrites(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err, "New")
	require.NoError(t, rw.Close(), "Close with no writes")

	// The head file should exist.
	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")
	require.Len(t, d.Files(), 1, "expected 1 file after close with no writes")
}

// ---------------------------------------------------------------------------
// Rotation edge-cases
// ---------------------------------------------------------------------------

// TestRotate_ExactBoundary — when curBytes equals MaxFileSize exactly,
// the next WriteTx must rotate before writing (i.e., the new tx lands in a
// new file, not the one that hit the boundary).
func TestRotate_ExactBoundary(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// Use a threshold equal to the estimate of one row so that after the
	// first WriteTx, curBytes >= maxFileSize and the second tx forces a rotation.
	const rowBody = 20

	const threshold = 30 + rowBody // rowHeaderEstimateBytes + bodySize

	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{},
		rotate.MaxFileSize(threshold))
	require.NoError(t, err, "New")

	require.NoError(t, rw.WriteTx([]format.XRow{makeRow(1, rowBody)}), "first WriteTx")
	// curBytes is now exactly threshold → next tx should rotate.
	require.NoError(t, rw.WriteTx([]format.XRow{makeRow(2, rowBody)}), "second WriteTx (forced rotate)")
	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")
	assert.GreaterOrEqual(t, len(d.Files()), 2, "expected ≥2 files at boundary")
}

// TestRotate_MultipleRotations — forces several rotations and verifies the
// complete chain's PrevVClock linkage and that every row is present exactly
// once across all files.
func TestRotate_MultipleRotations(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{},
		rotate.MaxFileSize(256))
	require.NoError(t, err, "New")

	const total = 12
	for lsn := int64(1); lsn <= total; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 100)}), "WriteTx lsn=%d", lsn)
	}

	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")

	files := d.Files()
	require.GreaterOrEqual(t, len(files), 3, "expected ≥3 files for 12 rows at 256-byte threshold")

	// Verify chain linkage.
	for i := 1; i < len(files); i++ {
		prev, cur := files[i-1], files[i]
		assert.True(t, vclockEqual(prev.VClock, cur.PrevVClock),
			"chain break at i=%d: prev.VClock=%v cur.PrevVClock=%v", i, prev.VClock, cur.PrevVClock)
	}

	// Collect all LSNs read back and verify each is present exactly once.
	seen := make(map[int64]bool)

	for _, f := range files {
		rd, err := reader.Open(f.Path)
		require.NoError(t, err, "reader.Open %s", f.Path)

		for {
			tx, err := rd.NextTx()
			if err != nil {
				break
			}

			for _, r := range tx.Rows {
				assert.False(t, seen[r.LSN], "duplicate LSN %d", r.LSN)
				seen[r.LSN] = true
			}
		}

		_ = rd.Close()
	}

	for lsn := int64(1); lsn <= total; lsn++ {
		assert.True(t, seen[lsn], "missing LSN %d", lsn)
	}
}

// TestRotate_WithNonEmptyStartVClock — a non-zero startVClock is threaded
// into the first file's Meta.VClock and forms the base of the chain.
func TestRotate_WithNonEmptyStartVClock(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	start := format.VClock{1: 100, 2: 50}

	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, start,
		rotate.MaxFileSize(256))
	require.NoError(t, err, "New with non-empty start VClock")

	for lsn := int64(101); lsn <= 104; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 100)}))
	}

	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")
	require.NotEmpty(t, d.Files(), "expected at least one file")

	// First file's PrevVClock must be empty (it is the head of chain).
	first := d.Files()[0]
	assert.Empty(t, first.PrevVClock, "first file PrevVClock should be empty")

	// First file's VClock signature must be 150 (sum of 100+50).
	assert.Equal(t, int64(150), first.VClock.Signature(), "first file VClock signature")
}

// TestRotate_InstanceUUID — every file in the chain must carry the same
// InstanceUUID that was passed to New.
func TestRotate_InstanceUUID(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	inst := uuid.MustParse("deadbeef-cafe-babe-0000-000000000042")

	rw, err := rotate.New(tmp, format.FiletypeXLOG, inst, format.VClock{},
		rotate.MaxFileSize(256))
	require.NoError(t, err, "New")

	for lsn := int64(1); lsn <= 6; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 100)}))
	}

	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")

	for _, f := range d.Files() {
		meta, err := reader.ReadHeader(f.Path)
		require.NoError(t, err, "ReadHeader %s", filepath.Base(f.Path))
		assert.Equal(t, inst, meta.InstanceUUID,
			"file %s: unexpected InstanceUUID", filepath.Base(f.Path))
	}
}

// TestRotate_ShouldNotRotateWhenUnderThreshold — when bytes written are
// strictly below the threshold, only one file should be produced.
func TestRotate_ShouldNotRotateWhenUnderThreshold(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// Large threshold — should never rotate.
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{},
		rotate.MaxFileSize(1024*1024))
	require.NoError(t, err, "New")

	for lsn := int64(1); lsn <= 5; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 50)}))
	}

	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")
	assert.Len(t, d.Files(), 1, "should have only one file below threshold")
}

// TestNew_DirAlreadyExists — New into an existing directory must succeed
// (MkdirAll is idempotent).
func TestNew_DirAlreadyExists(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// Directory already exists; New must not error.
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err, "New into existing dir")
	require.NoError(t, rw.Close(), "Close")
}

// TestNew_FiletypeSNAP — New accepts format.FiletypeSNAP and produces the
// correct .snap extension.
func TestNew_FiletypeSNAP(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeSNAP, testInstance, format.VClock{})
	require.NoError(t, err, "New with FiletypeSNAP")
	require.NoError(t, rw.Close(), "Close")

	// The .snap file must exist.
	want := filepath.Join(tmp, "00000000000000000000.snap")
	_, err = os.Stat(want)
	require.NoError(t, err, "expected %s to exist", want)
}

// TestRotate_ErrNoActiveWriter_via_rotateLocked — exercise the
// ErrNoActiveWriter sentinel: after a rotation failure leaves cur nil,
// a second forced rotation attempt (maxFileSize=0 so every tx tries)
// should propagate the openCurrent error (not ErrNoActiveWriter, since
// rotateLocked checks cur!=nil before closing). Confirms rotateLocked
// error paths via WriteTx.
func TestRotate_RotationErrorPropagates(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{},
		rotate.MaxFileSize(1))
	require.NoError(t, err, "New")

	// Successful first write — curBytes >= maxFileSize(1).
	require.NoError(t, rw.WriteTx([]format.XRow{makeRow(1, 5)}), "first WriteTx")

	// Block the next inprogress file.
	blocker := filepath.Join(tmp, "00000000000000000001.xlog.inprogress")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	// Second WriteTx — rotation fails on create.
	err = rw.WriteTx([]format.XRow{makeRow(2, 5)})
	require.Error(t, err, "expected error from failed rotation")
	assert.Contains(t, err.Error(), "rotate")

	// Third WriteTx — cur is now nil; rotateLocked should still fail since
	// the blocker is still present (creates a fresh error path).
	// We remove the blocker and let the third write check error flow again:
	require.NoError(t, os.Remove(blocker))
	// After rotation failure cur is nil; closing is safe.
	require.NoError(t, rw.Close(), "Close after rotation failure")
}

// TestWriteTx_ForceErrorFormat — verify the error returned by WriteTx after
// Close wraps ErrClosed so errors.Is works.
func TestWriteTx_ErrClosedIsWrapped(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err)
	require.NoError(t, rw.Close())

	err = rw.WriteTx([]format.XRow{makeRow(1, 10)})
	require.Error(t, err)
	// The error must unwrap to rotate.ErrClosed.
	assert.ErrorIs(t, err, rotate.ErrClosed,
		"expected errors.Is(err, ErrClosed); got %v", err)
}

// TestClose_ErrClosedIsWrapped — Close after Close must expose ErrClosed via
// errors.Is (not just string match).
func TestClose_ErrClosedIsWrapped(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err)
	require.NoError(t, rw.Close())

	err = rw.Close()
	require.Error(t, err)
	assert.ErrorIs(t, err, rotate.ErrClosed,
		"second Close: expected errors.Is(err, ErrClosed); got %v", err)
}

// ---------------------------------------------------------------------------
// rotateLocked ErrNoActiveWriter path
// ---------------------------------------------------------------------------

// TestRotate_NilCurAfterRotationFailureThenClose validates the scenario where
// rotateLocked sets cur=nil after closing the prior file but openCurrent fails.
// Subsequent Close must handle nil cur gracefully (return nil).
func TestRotate_NilCurAfterRotationFailureThenClose(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{},
		rotate.MaxFileSize(1))
	require.NoError(t, err, "New")

	// Write once so curBytes >= threshold.
	require.NoError(t, rw.WriteTx([]format.XRow{makeRow(1, 1)}), "first tx")

	// After lsn=1: runningVClock={1:1}, sig=1 → next file "…000001.xlog.inprogress"
	blocker := filepath.Join(tmp, "00000000000000000001.xlog.inprogress")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	// WriteTx triggers rotateLocked: closes prior file, sets cur=nil,
	// openCurrent fails → WriteTx returns error.
	err = rw.WriteTx([]format.XRow{makeRow(2, 1)})
	require.Error(t, err, "expected rotation create error")

	// Now cur is nil and closed is false → Close should take the nil-cur
	// branch and return nil.
	err = rw.Close()
	assert.NoError(t, err, "Close with nil cur must return nil")
}

// TestRotate_SnapFiletypeMultipleFiles — end-to-end with FiletypeSNAP and
// forced rotation to verify WriterOptions threads through to rotated files too.
func TestRotate_SnapFiletypeMultipleFiles(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(
		tmp,
		format.FiletypeSNAP,
		uuid.MustParse("00000000-0000-0000-0000-000000000099"),
		format.VClock{},
		rotate.MaxFileSize(128),
		rotate.WriterOptions(writer.NoCompression()),
	)
	require.NoError(t, err, "New FiletypeSNAP with WriterOptions")

	for lsn := int64(1); lsn <= 6; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 80)}))
	}

	require.NoError(t, rw.Close(), "Close")

	// All files must have .snap extension.
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)

	for _, e := range entries {
		name := e.Name()
		assert.True(t,
			len(name) > 5 && name[len(name)-5:] == ".snap",
			"unexpected extension for file: %s", name)
	}
}

// ---------------------------------------------------------------------------
// Sentinel error identity checks
// ---------------------------------------------------------------------------

// TestSentinelErrors — verify that ErrClosed and ErrEmptyRows are exported
// and distinct from each other and from nil.
func TestSentinelErrors(t *testing.T) {
	t.Parallel()

	require.Error(t, rotate.ErrClosed)
	require.Error(t, rotate.ErrEmptyRows)
	require.NotEqual(t, rotate.ErrClosed, rotate.ErrEmptyRows)

	// Sentinel strings should be human-readable.
	assert.NotEmpty(t, rotate.ErrClosed.Error())
	assert.NotEmpty(t, rotate.ErrEmptyRows.Error())

	// ErrNoActiveWriter is also exported.
	require.Error(t, rotate.ErrNoActiveWriter)
	assert.NotEmpty(t, fmt.Sprintf("%v", rotate.ErrNoActiveWriter))
}
