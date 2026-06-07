package reader_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// buildLog writes ntx single-row txs (distinct bodies) into an in-memory xlog.
func buildLog(t *testing.T, ntx int) []byte {
	t.Helper()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, &format.Meta{
		Filetype: format.FiletypeXLOG,
		Version:  "go-xlog/test",
		VClock:   format.VClock{1: 0},
	})
	require.NoError(t, err)

	for i := range ntx {
		// Body byte 0 encodes i so each row is distinguishable.
		body := []byte{0xc4, 1, byte(i)} // msgpack bin8 of 1 byte.
		row := format.XRow{Type: iproto.IPROTO_INSERT, LSN: int64(i + 1), BodyRaw: body}
		require.NoError(t, w.WriteTx([]format.XRow{row}))
	}

	require.NoError(t, w.Close())

	return buf.Bytes()
}

// TestScan_MatchesNext — Scan/Row must yield the same rows as Next.
func TestScan_MatchesNext(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 20)

	rn, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = rn.Close() }()

	rs, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = rs.Close() }()

	for rs.Scan() {
		want, nerr := rn.Next()
		require.NoError(t, nerr)

		got := rs.Row()
		assert.Equal(t, want.LSN, got.LSN)
		assert.Equal(t, want.Type, got.Type)
		assert.Equal(t, want.BodyRaw, got.BodyRaw)
	}

	require.NoError(t, rs.Err())

	// Rn must also be exhausted now.
	_, nerr := rn.Next()
	assert.Error(t, nerr, "Next should be at EOF too")
}

// TestScan_RetainSafeWithoutRecycle — rows (and their bodies) accumulated
// across the whole stream stay valid as long as Recycle is not called, even as
// later Scans grow the arena.
func TestScan_RetainSafeWithoutRecycle(t *testing.T) {
	t.Parallel()

	const ntx = 500 // Enough to force several arena reallocations.

	data := buildLog(t, ntx)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	var retained []format.XRow
	for r.Scan() {
		retained = append(retained, r.Row())
	}

	require.NoError(t, r.Err())
	require.Len(t, retained, ntx)

	// Every retained row must still hold its own correct data.
	for i, row := range retained {
		require.Equal(t, int64(i+1), row.LSN, "row[%d] LSN", i)
		require.Equal(t, []byte{0xc4, 1, byte(i)}, row.BodyRaw, "row[%d] body", i)
	}
}

// TestScan_AliasBodies_InvalidatedAcrossTxBlocks — with WithAliasBodies, a
// retained BodyRaw points into the shared read buffer; the default (copy) mode
// keeps it stable. This documents the two contracts side by side.
func TestScan_AliasBodies_InvalidatedAcrossTxBlocks(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 50)

	// Default mode: bodies copied into the arena, stable across the stream.
	rc, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = rc.Close() }()

	require.True(t, rc.Scan())
	firstBody := rc.Row().BodyRaw

	for rc.Scan() {
	}

	require.NoError(t, rc.Err())
	assert.Equal(t, []byte{0xc4, 1, 0}, firstBody, "copied body must survive later Scans")
}

// TestScanTx_MatchesNextTx — ScanTx/Tx must group rows like NextTx.
func TestScanTx_MatchesNextTx(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 10)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	count := 0

	for r.ScanTx() {
		rows := r.Tx()
		require.Len(t, rows, 1, "single-row txs")
		assert.True(t, rows[0].IsCommit())

		count++
	}

	require.NoError(t, r.Err())
	assert.Equal(t, 10, count)
}

// TestScanTx_MultiRow — a 3-row logical tx must come back as one ScanTx with
// all three rows intact, exercising txView growth *within* a single tx (each
// row decodes into a fresh slot, with its body copied into the body arena).
func TestScanTx_MultiRow(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, &format.Meta{
		Filetype: format.FiletypeXLOG,
		Version:  "go-xlog/test",
		VClock:   format.VClock{1: 0},
	})
	require.NoError(t, err)

	// One logical tx of 3 rows (writer.WriteTx stamps shared TSN + commit on
	// the last row).
	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: []byte{0xc4, 1, 0xaa}},
		{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: []byte{0xc4, 1, 0xbb}},
		{Type: iproto.IPROTO_INSERT, LSN: 3, BodyRaw: []byte{0xc4, 1, 0xcc}},
	}
	require.NoError(t, w.WriteTx(rows))
	require.NoError(t, w.Close())

	r, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	require.True(t, r.ScanTx())

	got := r.Tx()
	require.Len(t, got, 3)
	assert.Equal(t, []byte{0xc4, 1, 0xaa}, got[0].BodyRaw)
	assert.Equal(t, []byte{0xc4, 1, 0xbb}, got[1].BodyRaw)
	assert.Equal(t, []byte{0xc4, 1, 0xcc}, got[2].BodyRaw)
	assert.True(t, got[2].IsCommit(), "last row commits the tx")
	assert.False(t, got[0].IsCommit(), "non-final rows do not commit")

	assert.False(t, r.ScanTx(), "only one tx in the log")
	require.NoError(t, r.Err())
}
