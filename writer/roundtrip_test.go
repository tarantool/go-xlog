package writer //nolint:testpackage // shares internal test helpers (newMeta) with white-box tests in writer_test.go

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// TestRoundTrip_SingleStmtTxs — round-trips two single-stmt txs.
func TestRoundTrip_SingleStmtTxs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rt-single.xlog")

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1, 42})},
		{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: encodeDMLBody([]uint64{2, 84})},
	}

	w, err := Create(path, newMeta(t))
	require.NoError(t, err)

	for _, r := range rows {
		require.NoError(t, w.WriteTx([]format.XRow{r}))
	}

	require.NoError(t, w.Close())

	// Read back.
	rd, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = rd.Close() }()

	var got []format.XRow

	for {
		r, err := rd.Next()
		if err != nil {
			break
		}
		// BodyRaw aliases the reader's reusable scratch buffer; clone
		// before retaining across the next Next() call.
		body := make([]byte, len(r.BodyRaw))
		copy(body, r.BodyRaw)
		r.BodyRaw = body
		got = append(got, r)
	}

	require.Len(t, got, len(rows), "row count")

	for i, want := range rows {
		assert.Equal(t, want.Type, got[i].Type, "row[%d] Type", i)
		assert.Equal(t, want.LSN, got[i].LSN, "row[%d] LSN", i)
		assert.Equal(t, want.BodyRaw, got[i].BodyRaw, "row[%d] BodyRaw mismatch", i)
		// Single-stmt: decoder infers tsn=lsn, FlagCommit set.
		assert.Equal(t, got[i].LSN, got[i].TSN, "row[%d] TSN: want (=LSN)", i)
		assert.True(t, got[i].IsCommit(), "row[%d] should IsCommit() (single-stmt inferred)", i)
	}
}

// TestRoundTrip_MultiRowTx — tsn fixup + multi-row tx encoding.
//   - all rows share TSN == rows[0].LSN after read-back
//   - only the last row has IsCommit()
func TestRoundTrip_MultiRowTx(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rt-multi.xlog")

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 10, BodyRaw: encodeDMLBody([]uint64{1})},
		{Type: iproto.IPROTO_INSERT, LSN: 11, BodyRaw: encodeDMLBody([]uint64{2})},
		{Type: iproto.IPROTO_INSERT, LSN: 12, BodyRaw: encodeDMLBody([]uint64{3})},
	}

	w, err := Create(path, newMeta(t))
	require.NoError(t, err)
	require.NoError(t, w.WriteTx(rows))
	require.NoError(t, w.Close())

	rd, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = rd.Close() }()

	tx, err := rd.NextTx()
	require.NoError(t, err)
	require.Len(t, tx.Rows, 3, "tx row count")

	wantTSN := tx.Rows[0].LSN
	for i, r := range tx.Rows {
		assert.Equal(t, wantTSN, r.TSN, "row[%d] TSN", i)

		if i < len(tx.Rows)-1 {
			assert.False(t, r.IsCommit(), "row[%d] should not IsCommit()", i)
		}

		if i == len(tx.Rows)-1 {
			assert.True(t, r.IsCommit(), "row[%d] (last) should IsCommit()", i)
		}
	}
}

// TestRoundTrip_WriteRowCommitTx — WriteRow + CommitTx path.
func TestRoundTrip_WriteRowCommitTx(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rt-writerow.xlog")

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 100, BodyRaw: encodeDMLBody([]uint64{1})},
		{Type: iproto.IPROTO_INSERT, LSN: 101, BodyRaw: encodeDMLBody([]uint64{2})},
	}

	w, err := Create(path, newMeta(t))
	require.NoError(t, err)

	for _, r := range rows {
		require.NoError(t, w.WriteRow(r))
	}

	require.NoError(t, w.CommitTx())
	require.NoError(t, w.Close())

	rd, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = rd.Close() }()

	tx, err := rd.NextTx()
	require.NoError(t, err)
	require.Len(t, tx.Rows, 2, "tx row count")

	if tx.Rows[0].TSN != 100 || tx.Rows[1].TSN != 100 {
		t.Errorf("TSN mismatch: %d, %d", tx.Rows[0].TSN, tx.Rows[1].TSN)
	}

	assert.False(t, tx.Rows[0].IsCommit(), "row[0] should not IsCommit()")
	assert.True(t, tx.Rows[1].IsCommit(), "row[1] should IsCommit()")
}

// TestRoundTrip_CloseFlushesPendingTx — Close() with non-empty WriteRow
// buffer flushes the pending tx (do not lose data on Close).
func TestRoundTrip_CloseFlushesPendingTx(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rt-flushpending.xlog")
	w, err := Create(path, newMeta(t))
	require.NoError(t, err)
	require.NoError(t, w.WriteRow(format.XRow{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1})}))
	require.NoError(t, w.Close())

	rd, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = rd.Close() }()

	r, err := rd.Next()
	require.NoError(t, err)
	require.Equal(t, int64(1), r.LSN, "expected pending row to be flushed on Close")
}

// TestCompressionThreshold — payload >= CompressThreshold yields a
// zrow-marker tx; below threshold yields row-marker.
func TestCompressionThreshold(t *testing.T) {
	t.Parallel()

	t.Run("BigPayloadCompressed", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "big.xlog")
		w, err := Create(path, newMeta(t))
		require.NoError(t, err)

		row := format.XRow{
			Type:    iproto.IPROTO_INSERT,
			LSN:     1,
			BodyRaw: fixedDMLBody(512, 4*1024), // 4 KiB blob → over threshold.
		}
		require.NoError(t, w.WriteTx([]format.XRow{row}))
		require.NoError(t, w.Close())

		magic := firstTxMagic(t, path)
		require.True(t, bytes.Equal(magic[:], format.ZRowMarker[:]), "expected ZRowMarker, got % x", magic)
	})

	t.Run("SmallPayloadUncompressed", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "small.xlog")
		w, err := Create(path, newMeta(t))
		require.NoError(t, err)

		row := format.XRow{
			Type:    iproto.IPROTO_INSERT,
			LSN:     1,
			BodyRaw: encodeDMLBody([]uint64{1, 2}),
		}
		require.NoError(t, w.WriteTx([]format.XRow{row}))
		require.NoError(t, w.Close())
		magic := firstTxMagic(t, path)
		require.True(t, bytes.Equal(magic[:], format.RowMarker[:]), "expected RowMarker, got % x", magic)
	})

	t.Run("NoCompressionOption", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "nocompress.xlog")
		w, err := Create(path, newMeta(t), NoCompression())
		require.NoError(t, err)

		row := format.XRow{
			Type:    iproto.IPROTO_INSERT,
			LSN:     1,
			BodyRaw: fixedDMLBody(512, 4*1024),
		}
		require.NoError(t, w.WriteTx([]format.XRow{row}))
		require.NoError(t, w.Close())
		magic := firstTxMagic(t, path)
		require.True(t, bytes.Equal(magic[:], format.RowMarker[:]), "NoCompression: expected RowMarker, got % x", magic)
	})
}

// TestEOFMarker — Close() writes the 4-byte EOF marker as the file's last bytes.
func TestEOFMarker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "eof.xlog")
	w, err := Create(path, newMeta(t))
	require.NoError(t, err)
	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1})},
	}))
	require.NoError(t, w.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(raw), 4, "file too short: %d bytes", len(raw))
	tail := raw[len(raw)-4:]
	require.True(t, bytes.Equal(tail, format.EOFMarker[:]), "tail bytes are not EOF marker: % x", tail)
}

// firstTxMagic reads the file, skips the meta header (everything up to and
// including the blank-line terminator), and returns the first 4 bytes after
// it (the magic of tx block 1).
func firstTxMagic(t *testing.T, path string) [4]byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read %q", path)
	// Meta header ends at the first occurrence of two consecutive newlines
	// (the blank-line terminator).
	idx := bytes.Index(raw, []byte("\n\n"))
	require.GreaterOrEqual(t, idx, 0, "meta terminator not found in file %q", path)
	rest := raw[idx+2:]
	require.GreaterOrEqual(t, len(rest), 4, "not enough bytes after meta for a magic")

	var m [4]byte
	copy(m[:], rest[:4])

	return m
}
