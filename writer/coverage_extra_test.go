package writer_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// errBoom is the sentinel returned by failWriter once its byte budget is spent.
var errBoom = errors.New("boom")

// failWriter is an io.Writer that accepts a fixed number of bytes, then fails
// every subsequent Write. It lets black-box tests drive the Writer's write /
// flush error branches (which otherwise need a failing fd) through the public
// NewWriter(io.Writer) seam. budget < 0 means "fail immediately"; a large budget
// lets the meta header through but trips on the first big tx payload.
type failWriter struct {
	budget int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.budget <= 0 {
		return 0, errBoom
	}

	if len(p) <= f.budget {
		f.budget -= len(p)

		return len(p), nil
	}

	n := f.budget
	f.budget = 0

	return n, errBoom
}

// fileMeta clones exampleMeta but allows a per-test instance so file-based
// tests run in parallel TempDirs without clobbering shared state.
func fileMeta() *format.Meta { return exampleMeta() }

// TestSync_FileWriter exercises Sync() on a real file writer for both the
// default (SyncNormal → f.Sync) and SyncDataSync (→ fdatasync, which falls back
// to f.Sync() on darwin) policies, then confirms the file still finalises and
// round-trips. This is the only path that drives Sync/syncFile/fdatasync.
func TestSync_FileWriter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode writer.SyncMode
	}{
		{"SyncNormal", writer.SyncNormal},
		{"SyncDataSync", writer.SyncDataSync},
		{"SyncNone", writer.SyncNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "00000000000000000001.xlog")

			w, err := writer.Create(path, fileMeta(), writer.Sync(tc.mode))
			require.NoError(t, err)

			require.NoError(t, w.WriteTx([]format.XRow{
				{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)},
			}))

			// Explicit mid-stream Sync flushes the bufio + issues the kernel sync.
			require.NoError(t, w.Sync())

			require.NoError(t, w.WriteTx([]format.XRow{
				{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: exampleBody(2)},
			}))

			require.NoError(t, w.Close())

			r, err := reader.Open(path)
			require.NoError(t, err)

			defer func() { _ = r.Close() }()

			n := 0
			for _, err := range r.Rows() {
				require.NoError(t, err)

				n++
			}

			assert.Equal(t, 2, n, "both txs round-trip")
		})
	}
}

// TestSync_InMemoryNoop confirms Sync() on a NewWriter (no fd) flushes the
// buffer but performs no kernel sync (syncFile's f == nil branch).
func TestSync_InMemoryNoop(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, exampleMeta())
	require.NoError(t, err)

	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)},
	}))
	require.NoError(t, w.Sync())
	require.NoError(t, w.Close())

	require.NotZero(t, buf.Len())
}

// TestSync_AfterClose returns ErrClosed.
func TestSync_AfterClose(t *testing.T) {
	t.Parallel()

	w, err := writer.NewWriter(&bytes.Buffer{}, exampleMeta())
	require.NoError(t, err)
	require.NoError(t, w.Close())

	require.ErrorIs(t, w.Sync(), writer.ErrClosed)
}

// TestCreate_FullLifecycle drives Create + WriteRow + CommitTx + Sync + Close
// (default SyncNormal) over a real file and round-trips the result, covering the
// happy file path through writeFramed (uncompressed) and the directory fsync.
func TestCreate_FullLifecycle(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "00000000000000000005.xlog")

	w, err := writer.Create(path, fileMeta())
	require.NoError(t, err)

	// WriteRow + explicit CommitTx (single-row API).
	require.NoError(t, w.WriteRow(format.XRow{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)}))
	require.NoError(t, w.WriteRow(format.XRow{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: exampleBody(2)}))
	require.NoError(t, w.CommitTx())

	require.NoError(t, w.Sync())
	require.NoError(t, w.Close())

	r, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	txs := 0
	for _, err := range r.Txs() {
		require.NoError(t, err)

		txs++
	}

	assert.Equal(t, 1, txs, "two rows committed as one tx")
}

// TestWriteRow_AutoCommitOnThreshold: WriteRow auto-flushes when a commit-marked
// row pushes the buffered payload estimate at/above AutocommitThreshold. This is
// the only branch in WriteRow that calls CommitTx implicitly.
func TestWriteRow_AutoCommitOnThreshold(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, exampleMeta())
	require.NoError(t, err)

	// One row larger than the 128 KiB autocommit threshold, marked commit.
	big := bigBody(format.AutocommitThreshold + 16)
	require.NoError(t, w.WriteRow(format.XRow{
		Type:    iproto.IPROTO_INSERT,
		LSN:     1,
		Flags:   iproto.IPROTO_FLAG_COMMIT,
		BodyRaw: big,
	}))

	// The auto-flush already wrote the tx; a follow-up CommitTx is a no-op.
	require.NoError(t, w.CommitTx())
	require.NoError(t, w.Close())

	r, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	n := 0
	for _, err := range r.Rows() {
		require.NoError(t, err)

		n++
	}

	assert.Equal(t, 1, n)
}

// TestWriteTx_Errors covers the WriteTx guard branches reachable via the public
// API: empty rows and a pending WriteRow accumulator.
func TestWriteTx_Errors(t *testing.T) {
	t.Parallel()

	t.Run("empty rows", func(t *testing.T) {
		t.Parallel()

		w, err := writer.NewWriter(&bytes.Buffer{}, exampleMeta())
		require.NoError(t, err)

		require.ErrorIs(t, w.WriteTx(nil), writer.ErrEmptyRows)
		require.ErrorIs(t, w.WriteTx([]format.XRow{}), writer.ErrEmptyRows)
	})

	t.Run("pending write row", func(t *testing.T) {
		t.Parallel()

		w, err := writer.NewWriter(&bytes.Buffer{}, exampleMeta())
		require.NoError(t, err)

		require.NoError(t, w.WriteRow(format.XRow{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)}))
		require.ErrorIs(t, w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: exampleBody(2)}}), writer.ErrPendingWriteRow)
	})

	t.Run("after close", func(t *testing.T) {
		t.Parallel()

		w, err := writer.NewWriter(&bytes.Buffer{}, exampleMeta())
		require.NoError(t, err)
		require.NoError(t, w.Close())

		require.ErrorIs(t, w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)}}), writer.ErrClosed)
	})
}

// TestCreate_NilMeta and TestNewWriter guards.
func TestConstructorGuards(t *testing.T) {
	t.Parallel()

	t.Run("Create nil meta", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "x.xlog")
		_, err := writer.Create(path, nil)
		require.ErrorIs(t, err, writer.ErrNilMeta)
	})

	t.Run("NewWriter nil dst", func(t *testing.T) {
		t.Parallel()

		_, err := writer.NewWriter(nil, exampleMeta())
		require.ErrorIs(t, err, writer.ErrNilDst)
	})

	t.Run("NewWriter nil meta", func(t *testing.T) {
		t.Parallel()

		_, err := writer.NewWriter(&bytes.Buffer{}, nil)
		require.ErrorIs(t, err, writer.ErrNilMeta)
	})
}

// TestCreate_MetaEncodeError: Create opens the .inprogress, then EncodeMeta
// fails on an invalid meta (empty Filetype). Create must roll back — close the
// fd and remove the partial .inprogress (covers Create's encode-meta branch).
func TestCreate_MetaEncodeError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000042.xlog")

	bad := &format.Meta{InstanceUUID: exampleMeta().InstanceUUID} // empty Filetype
	_, err := writer.Create(path, bad)
	require.Error(t, err)

	// The partial .inprogress must NOT be left behind.
	_, statErr := os.Stat(path + ".inprogress")
	require.True(t, os.IsNotExist(statErr), "rollback should remove .inprogress")
}

// TestWriteRawBlock_WriteError: a valid (large) raw block written to a failing
// io.Writer surfaces the write error (covers WriteRawBlock's write-error
// branch).
func TestWriteRawBlock_WriteError(t *testing.T) {
	t.Parallel()

	// First build one valid framed block by capturing a normal write.
	var captured bytes.Buffer

	cw, err := writer.NewWriter(&captured, exampleMeta(), writer.NoCompression())
	require.NoError(t, err)
	require.NoError(t, cw.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: bigBody(bufioSize)},
	}))
	require.NoError(t, cw.Close())

	// Extract the first raw block (fixheader+payload) right after the header.
	r, err := reader.NewReader(bytes.NewReader(captured.Bytes()))
	require.NoError(t, err)

	block, err := r.NextBlockRaw()
	require.NoError(t, err)

	rawBlock := append([]byte(nil), block...)
	_ = r.Close()

	// Now write it to a failing writer; the oversized block flushes through.
	fw := &failWriter{budget: 4096}

	w, err := writer.NewWriter(fw, exampleMeta())
	require.NoError(t, err)

	require.ErrorIs(t, w.WriteRawBlock(rawBlock), errBoom)
}

// TestCreate_BadDirectory: Create against a path whose parent directory does
// not exist fails on the O_EXCL OpenFile, covering Create's error return.
func TestCreate_BadDirectory(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "no-such-subdir", "x.xlog")
	_, err := writer.Create(missing, exampleMeta())
	require.Error(t, err)
}

// TestDiscard_FileWriter covers Discard on a real file: it closes the fd and
// removes the .inprogress without promoting it, and subsequent calls return
// ErrClosed.
func TestDiscard_FileWriter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000009.xlog")
	inprog := path + ".inprogress"

	w, err := writer.Create(path, fileMeta())
	require.NoError(t, err)

	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)},
	}))
	require.NoError(t, w.Discard())

	_, err = os.Stat(inprog)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))

	// Post-discard calls all return ErrClosed.
	require.ErrorIs(t, w.Discard(), writer.ErrClosed)
	require.ErrorIs(t, w.Close(), writer.ErrClosed)
	require.ErrorIs(t, w.Sync(), writer.ErrClosed)
}

// TestDiscard_InMemory covers Discard on a NewWriter: no .inprogress file to
// remove, returns nil.
func TestDiscard_InMemory(t *testing.T) {
	t.Parallel()

	w, err := writer.NewWriter(&bytes.Buffer{}, exampleMeta())
	require.NoError(t, err)

	require.NoError(t, w.Discard())
	require.ErrorIs(t, w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)}}), writer.ErrClosed)
}

// TestDiscard_RemoveError: removing the .inprogress out from under an open file
// Writer makes Discard's os.Remove fail, covering its remove-error branch. The
// Writer still transitions to closed.
func TestDiscard_RemoveError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000088.xlog")
	inprog := path + ".inprogress"

	w, err := writer.Create(path, fileMeta())
	require.NoError(t, err)

	require.NoError(t, os.Remove(inprog)) // Yank the file the fd points at.

	err = w.Discard()
	require.Error(t, err) // remove of a now-missing file fails.
	require.ErrorIs(t, w.Discard(), writer.ErrClosed)
}

// TestClose_EmptyFileWriter closes a Create'd writer with no rows written: the
// CommitTx no-op, EOF marker, flush, sync, close, rename, dir-fsync chain all
// run on an otherwise empty file.
func TestClose_EmptyFileWriter(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "00000000000000000010.xlog")

	w, err := writer.Create(path, fileMeta())
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	n := 0
	for _, err := range r.Rows() {
		require.NoError(t, err)

		n++
	}

	assert.Zero(t, n)
}

// TestBatchWriter_FileLifecycle drives the BatchWriter over a file writer with a
// pending block at Close (Flush via Close) plus an explicit Flush, exercising
// writeBlockPayload through the file path and round-trips the output.
func TestBatchWriter_FileLifecycle(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "00000000000000000011.xlog")

	w, err := writer.Create(path, fileMeta(), writer.Sync(writer.SyncDataSync))
	require.NoError(t, err)

	bw := writer.NewBatchWriter(w, writer.BatchOptions{MaxTxs: 3})

	for i := range 7 {
		require.NoError(t, bw.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, LSN: int64(i + 1), BodyRaw: bigBody(4096)},
		}))
	}

	// One explicit flush mid-stream (flushes a partial pending block, if any),
	// then Close flushes the trailing block.
	require.NoError(t, bw.Flush())
	require.NoError(t, w.Sync())
	require.NoError(t, bw.Close())

	r, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	n := 0
	for _, err := range r.Rows() {
		require.NoError(t, err)

		n++
	}

	assert.Equal(t, 7, n)
}

// TestBatchWriter_FlushWriteError: a large pending block flushed to a failing
// writer surfaces the error and leaves the pending buffer intact (Flush's
// error-return branch + writeBlockPayload's write through writeFramed).
func TestBatchWriter_FlushWriteError(t *testing.T) {
	t.Parallel()

	fw := &failWriter{budget: 4096}

	w, err := writer.NewWriter(fw, exampleMeta(), writer.NoCompression())
	require.NoError(t, err)

	bw := writer.NewBatchWriter(w, writer.BatchOptions{}) // never auto-flushes

	require.NoError(t, bw.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: bigBody(bufioSize)},
	}))

	// Explicit flush of the oversized pending block trips the failing writer.
	require.ErrorIs(t, bw.Flush(), errBoom)
}

// TestBatchWriter_CloseFlushError: Close flushes the trailing block first; when
// that flush fails Close returns the error and leaves the Writer open (covers
// BatchWriter.Close's flush-error branch).
func TestBatchWriter_CloseFlushError(t *testing.T) {
	t.Parallel()

	fw := &failWriter{budget: 4096}

	w, err := writer.NewWriter(fw, exampleMeta(), writer.NoCompression())
	require.NoError(t, err)

	bw := writer.NewBatchWriter(w, writer.BatchOptions{})

	require.NoError(t, bw.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: bigBody(bufioSize)},
	}))

	require.ErrorIs(t, bw.Close(), errBoom)
}

// TestBatchWriter_FlushAfterUnderlyingClosed: closing the underlying Writer
// directly, then flushing a pending batch, hits writeBlockPayload's ErrClosed
// guard.
func TestBatchWriter_FlushAfterUnderlyingClosed(t *testing.T) {
	t.Parallel()

	w, err := writer.NewWriter(&bytes.Buffer{}, exampleMeta())
	require.NoError(t, err)

	bw := writer.NewBatchWriter(w, writer.BatchOptions{}) // no auto-flush

	require.NoError(t, bw.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)},
	}))

	// Close the underlying writer out from under the batch; the pending block is
	// still buffered, so Flush must report the writer is closed.
	require.NoError(t, w.Close())
	require.ErrorIs(t, bw.Flush(), writer.ErrClosed)
}

// TestClose_RenameError: removing the .inprogress file out from under an open
// file Writer makes the final rename fail, covering Close's rename-error branch.
func TestClose_RenameError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000077.xlog")
	inprog := path + ".inprogress"

	w, err := writer.Create(path, fileMeta())
	require.NoError(t, err)

	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: exampleBody(1)},
	}))

	// Yank the .inprogress before Close: fd writes still work, but the rename of
	// a now-missing source fails.
	require.NoError(t, os.Remove(inprog))

	err = w.Close()
	require.Error(t, err)
	require.ErrorIs(t, w.Close(), writer.ErrClosed)
}

// TestBatchWriter_EmptyTx covers the BatchWriter.WriteTx empty-rows guard.
func TestBatchWriter_EmptyTx(t *testing.T) {
	t.Parallel()

	w, err := writer.NewWriter(&bytes.Buffer{}, exampleMeta())
	require.NoError(t, err)

	bw := writer.NewBatchWriter(w, writer.BatchOptions{MaxTxs: 2})
	require.ErrorIs(t, bw.WriteTx(nil), writer.ErrEmptyRows)
	require.ErrorIs(t, bw.WriteTx([]format.XRow{}), writer.ErrEmptyRows)
}

// TestVersionDefault_NewWriter: with no Version on the meta and no Version
// option, resolveCfg fills the "go-xlog/0.1" default, which lands in the
// on-disk header.
func TestVersionDefault_NewWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	m := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		InstanceUUID: exampleMeta().InstanceUUID,
	}

	w, err := writer.NewWriter(&buf, m)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	assert.Contains(t, buf.String(), "Version: go-xlog/0.1\n")
}

// bufioSize must exceed the writer's internal bufio buffer (AutocommitThreshold)
// so a single payload write flushes straight through to the underlying io.Writer
// and surfaces its error.
const bufioSize = format.AutocommitThreshold + 64*1024

// TestNewWriter_MetaEncodeError: an invalid meta (empty Filetype) makes
// format.EncodeMeta fail, covering NewWriter's encode-meta error branch.
func TestNewWriter_MetaEncodeError(t *testing.T) {
	t.Parallel()

	bad := &format.Meta{InstanceUUID: exampleMeta().InstanceUUID} // no Filetype
	_, err := writer.NewWriter(&bytes.Buffer{}, bad)
	require.Error(t, err)
}

// TestWriteTx_WriteError forces the underlying io.Writer to fail while flushing
// a large tx payload, covering the write-error returns in writeFramed /
// encodeAndWriteTx.
func TestWriteTx_WriteError(t *testing.T) {
	t.Parallel()

	// Budget lets the small meta header through, then trips on the big payload.
	fw := &failWriter{budget: 4096}

	w, err := writer.NewWriter(fw, exampleMeta(), writer.NoCompression())
	require.NoError(t, err)

	err = w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: bigBody(bufioSize)},
	})
	require.ErrorIs(t, err, errBoom)
}

// TestWriteTx_CompressWriteError drives the compressed branch of writeFramed:
// an incompressible body keeps the framed block large enough to flush straight
// through the bufio buffer, so the failing writer trips on the ZRow payload.
func TestWriteTx_CompressWriteError(t *testing.T) {
	t.Parallel()

	fw := &failWriter{budget: 4096}

	// Default policy compresses (threshold 2 KiB). Random bytes do not shrink, so
	// the compressed block still exceeds the bufio buffer and flushes through.
	w, err := writer.NewWriter(fw, exampleMeta())
	require.NoError(t, err)

	err = w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: incompressibleBody(bufioSize)},
	})
	require.ErrorIs(t, err, errBoom)
}

// TestSync_FlushError: Sync surfaces a bufio.Flush error from the underlying
// writer (covers Sync's flush-error return).
func TestSync_FlushError(t *testing.T) {
	t.Parallel()

	fw := &failWriter{budget: 4096}

	w, err := writer.NewWriter(fw, exampleMeta(), writer.NoCompression())
	require.NoError(t, err)

	// Buffer a payload larger than the bufio buffer is impossible without a
	// flush; instead buffer a small tx (stays in bufio) then Sync, which flushes
	// the meta+tx tail through the failing writer once its budget is spent.
	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: bigBody(8192)},
	}))

	err = w.Sync()
	require.ErrorIs(t, err, errBoom)
}

// TestClose_FlushError: Close surfaces the bufio.Flush failure of a buffered
// tail (covers Close's flush-error branch + releaseFile no-op for the in-memory
// writer).
func TestClose_FlushError(t *testing.T) {
	t.Parallel()

	fw := &failWriter{budget: 4096}

	w, err := writer.NewWriter(fw, exampleMeta(), writer.NoCompression())
	require.NoError(t, err)

	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: bigBody(8192)},
	}))

	err = w.Close()
	require.ErrorIs(t, err, errBoom)
	// Writer is now closed; further calls return ErrClosed.
	require.ErrorIs(t, w.Close(), writer.ErrClosed)
}

// TestClose_CommitTxError: a pending WriteRow accumulator whose flush fails at
// Close surfaces the error through Close's step-1 CommitTx (covers the
// CommitTx-error branch of Close).
func TestClose_CommitTxError(t *testing.T) {
	t.Parallel()

	fw := &failWriter{budget: 4096}

	w, err := writer.NewWriter(fw, exampleMeta(), writer.NoCompression())
	require.NoError(t, err)

	// A non-commit row stays buffered (no auto-flush), so it flushes at Close.
	require.NoError(t, w.WriteRow(format.XRow{
		Type:    iproto.IPROTO_INSERT,
		LSN:     1,
		BodyRaw: bigBody(bufioSize),
	}))

	err = w.Close()
	require.ErrorIs(t, err, errBoom)
}

// incompressibleBody returns a msgpack-bin body of n pseudo-random bytes that
// zstd cannot shrink, so the framed compressed block stays larger than the
// writer's bufio buffer. Deterministic (seeded LCG) for reproducibility.
func incompressibleBody(n int) []byte {
	raw := make([]byte, n)

	var s uint64 = 0x9e3779b97f4a7c15
	for i := range raw {
		s = s*6364136223846793005 + 1442695040888963407
		raw[i] = byte(s >> 33)
	}

	// Wrap as a one-element msgpack array holding a bin so the reader/encoder
	// treats it as a valid body, but we never need to read it back here.
	return appendBinBody(raw)
}

// appendBinBody frames raw as msgpack: [ bin(raw) ] (array of one bin).
func appendBinBody(raw []byte) []byte {
	b := make([]byte, 0, len(raw)+8)
	b = append(b, 0x91)             // fixarray, 1 element
	b = append(b, 0xc6)             // bin32
	n := uint32(len(raw))           //nolint:gosec
	b = append(b, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(b, raw...)
}
