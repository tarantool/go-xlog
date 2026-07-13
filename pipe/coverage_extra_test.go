package pipe_test

// coverage_extra_test.go adds targeted tests for the error branches in Copy
// that were not covered by the existing suite:
//
//   - Copy: nil src guard (ErrNilSrcReader)
//   - Copy: nil dst guard (ErrNilDstWriter)
//   - Copy: non-EOF reader error mid-stream
//   - Copy: writer error mid-stream
//   - CopyRaw: writer error mid-stream

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/pipe"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// TestCopy_NilSrc verifies that Copy returns ErrNilSrcReader when src is nil.
func TestCopy_NilSrc(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, newMeta())
	require.NoError(t, err)

	_, copyErr := pipe.Copy(nil, w)
	require.ErrorIs(t, copyErr, pipe.ErrNilSrcReader)
}

// TestCopy_NilDst verifies that Copy returns ErrNilDstWriter when dst is nil.
func TestCopy_NilDst(t *testing.T) {
	t.Parallel()

	src := buildInMemSource(t)

	r, err := reader.NewReader(bytes.NewReader(src))
	require.NoError(t, err)

	_, copyErr := pipe.Copy(r, nil)
	require.ErrorIs(t, copyErr, pipe.ErrNilDstWriter)
}

// TestCopy_ReaderErrorMidStream verifies that a non-EOF reader error is surfaced
// and wrapped by Copy. The source is corrupted mid-stream (second block) so the
// first tx block has an invalid CRC; with no SkipCorruptTx the reader returns
// ErrCorruptCRC on the next NextTx call.
func TestCopy_ReaderErrorMidStream(t *testing.T) {
	t.Parallel()

	// Build a source with multiple blocks, then corrupt the first block so
	// NextTx() returns a CRC error immediately (before any tx is written).
	src := buildInMemSource(t)
	corruptFirstPayloadByte(t, src)

	r, err := reader.NewReader(bytes.NewReader(src))
	require.NoError(t, err)

	var dstBuf bytes.Buffer

	w, err := writer.NewWriter(&dstBuf, newMeta())
	require.NoError(t, err)

	_, copyErr := pipe.Copy(r, w)
	require.Error(t, copyErr, "Copy must fail on a corrupt source")
	assert.ErrorIs(t, copyErr, reader.ErrCorruptCRC, "want ErrCorruptCRC wrapped by pipe.Copy")
}

// TestCopy_WriterError verifies that a Write-time failure from dst.WriteTx is
// surfaced by Copy. We close the writer before calling Copy so that the first
// WriteTx call inside Copy returns ErrClosed.
func TestCopy_WriterError(t *testing.T) {
	t.Parallel()

	src := buildInMemSource(t)

	r, err := reader.NewReader(bytes.NewReader(src))
	require.NoError(t, err)

	var dstBuf bytes.Buffer

	w, err := writer.NewWriter(&dstBuf, newMeta())
	require.NoError(t, err)

	// Close the writer so WriteTx returns ErrClosed on the first call.
	require.NoError(t, w.Close())

	_, copyErr := pipe.Copy(r, w)
	require.Error(t, copyErr, "Copy must fail when the writer is closed")
	assert.ErrorIs(t, copyErr, writer.ErrClosed, "want ErrClosed wrapped by pipe.Copy")
}

// TestCopyRaw_WriterError verifies that a WriteRawBlock error is surfaced by
// CopyRaw. We close the writer before calling CopyRaw so that the first
// WriteRawBlock call returns ErrClosed.
func TestCopyRaw_WriterError(t *testing.T) {
	t.Parallel()

	src := buildInMemSource(t)

	r, err := reader.NewReader(bytes.NewReader(src))
	require.NoError(t, err)

	var dstBuf bytes.Buffer

	w, err := writer.NewWriter(&dstBuf, newMeta())
	require.NoError(t, err)

	// Close the writer so WriteRawBlock returns ErrClosed.
	require.NoError(t, w.Close())

	_, rawErr := pipe.CopyRaw(r, w)
	require.Error(t, rawErr, "CopyRaw must fail when the writer is closed")
	assert.ErrorIs(t, rawErr, writer.ErrClosed, "want ErrClosed wrapped by pipe.CopyRaw")
}

// buildInMemSource writes a small in-memory xlog with three single-row txs
// (LSNs 1-3) and returns the raw bytes. It is the in-memory analogue of the
// file-based buildSource used in copy_test.go, suitable for corruption tests.
func buildInMemSource(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, newMeta())
	require.NoError(t, err, "NewWriter")

	for lsn := int64(1); lsn <= 3; lsn++ {
		row := format.XRow{
			Type:      iproto.IPROTO_INSERT,
			ReplicaID: 1,
			LSN:       lsn,
			BodyRaw:   dmlBody(uint64(lsn), 100),
		}
		require.NoError(t, w.WriteTx([]format.XRow{row}), "WriteTx lsn=%d", lsn)
	}

	require.NoError(t, w.Close(), "Close")

	return buf.Bytes()
}
