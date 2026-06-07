package writer //nolint:testpackage // shares internal test helpers (newMeta) with white-box tests in writer_test.go

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// TestNewWriter_InMemory exercises the io.Writer-based constructor: it streams
// to a bytes.Buffer (no file, no fsync, no rename) and the bytes read back
// through the reader match what was written, terminated by the EOF marker.
func TestNewWriter_InMemory(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w, err := NewWriter(&buf, newMeta(t))
	require.NoError(t, err)

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1, 42})},
		{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: encodeDMLBody([]uint64{2, 84})},
	}
	for _, r := range rows {
		require.NoError(t, w.WriteTx([]format.XRow{r}))
	}

	require.NoError(t, w.Close())

	assert.True(t, bytes.HasSuffix(buf.Bytes(), format.EOFMarker[:]), "in-memory output does not end with the EOF marker")

	rd, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)

	defer func() { _ = rd.Close() }()

	n := 0

	for row, err := range rd.Rows() {
		require.NoError(t, err, "Rows")
		assert.Equal(t, rows[n].LSN, row.LSN, "row[%d] LSN", n)
		n++
	}

	assert.Equal(t, len(rows), n, "row count")

	// Post-Close calls return ErrClosed; Discard is a no-op (no file).
	assert.ErrorIs(t, w.WriteTx(rows), ErrClosed, "post-Close WriteTx")
}

// TestNewWriter_NilDst rejects a nil destination.
func TestNewWriter_NilDst(t *testing.T) {
	t.Parallel()

	_, err := NewWriter(nil, newMeta(t))
	assert.Error(t, err, "NewWriter(nil) = nil error, want non-nil")
}
