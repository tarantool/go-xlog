package format_test

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
)

// fixtureWalk walks a full file: DecodeMeta, then iterate tx blocks until
// the EOF marker, returning per-tx and per-row counts plus any compressed
// magic seen.
type fixtureStats struct {
	txCount       int
	rowCount      int
	maxRowsPerTx  int
	sawCompressed bool
	rowTypes      map[iproto.Type]int
}

func walkFixture(t *testing.T, name string) fixtureStats {
	t.Helper()
	data := testutil.Load(t, name)
	r := bufio.NewReader(bytes.NewReader(data))
	_, err := format.DecodeMeta(r, format.MetaOptions{})
	require.NoErrorf(t, err, "%s: DecodeMeta", name)
	// Find the offset just after the meta blank-line terminator: bufio
	// has now consumed the meta header. We need an offset into the raw
	// data to feed DecodeTxBlock. Compute by scanning the raw bytes.
	off := 0
	for ; off+1 < len(data); off++ {
		if data[off] == '\n' && data[off+1] == '\n' {
			off += 2

			break
		}
	}

	stats := fixtureStats{rowTypes: map[iproto.Type]int{}}

	for off < len(data) {
		if off+4 <= len(data) && bytes.Equal(data[off:off+4], format.EOFMarker[:]) {
			off += 4

			break
		}

		slices, _, n, err := format.DecodeTxBlock(data[off:])
		require.NoErrorf(t, err, "%s: tx at offset %d", name, off)

		if data[off+2] == format.ZRowMarker[2] && data[off+3] == format.ZRowMarker[3] {
			stats.sawCompressed = true
		}

		stats.txCount++
		if len(slices) > stats.maxRowsPerTx {
			stats.maxRowsPerTx = len(slices)
		}

		for _, s := range slices {
			row, _, err := format.DecodeXRow(s)
			require.NoErrorf(t, err, "%s: row decode in tx at offset %d", name, off)

			stats.rowCount++
			stats.rowTypes[row.Type]++
		}

		off += n
	}

	assert.Equalf(t, len(data), off, "%s: trailing %d bytes after EOF marker", name, len(data)-off)

	return stats
}

func TestFixture_SimpleXlog(t *testing.T) {
	t.Parallel()

	s := walkFixture(t, "simple.xlog")
	require.NotZero(t, s.txCount, "expected at least 1 tx in simple.xlog")
	require.NotZero(t, s.rowCount, "expected at least 1 row in simple.xlog")
	// Per README magic-byte inventory, simple.xlog has one compressed tx.
	assert.Truef(t, s.sawCompressed, "expected at least one ZRowMarker tx in simple.xlog; tx=%d rows=%d", s.txCount, s.rowCount)
	t.Logf("simple.xlog: tx=%d rows=%d maxRowsPerTx=%d types=%v compressed=%v",
		s.txCount, s.rowCount, s.maxRowsPerTx, s.rowTypes, s.sawCompressed)
}

func TestFixture_MultistmtXlog(t *testing.T) {
	t.Parallel()

	s := walkFixture(t, "multistmt.xlog")
	assert.GreaterOrEqualf(t, s.maxRowsPerTx, 2, "multistmt.xlog: expected at least one tx with ≥2 rows, max=%d", s.maxRowsPerTx)
}

func TestFixture_CompressedXlog(t *testing.T) {
	t.Parallel()

	s := walkFixture(t, "compressed.xlog")
	assert.True(t, s.sawCompressed, "compressed.xlog: expected at least one ZRowMarker tx")
}

func TestFixture_EmptySnap(t *testing.T) {
	t.Parallel()

	s := walkFixture(t, "empty.snap")
	assert.NotZero(t, s.rowCount, "empty.snap should have system-space rows even on bootstrap; got 0")
	t.Logf("empty.snap: tx=%d rows=%d types=%v", s.txCount, s.rowCount, s.rowTypes)
}

func TestFixture_PopulatedSnap(t *testing.T) {
	t.Parallel()

	s := walkFixture(t, "populated.snap")
	assert.NotZero(t, s.rowCount, "populated.snap should have rows; got 0")
	t.Logf("populated.snap: tx=%d rows=%d types=%v", s.txCount, s.rowCount, s.rowTypes)
}

func TestFixture_VyLog(t *testing.T) {
	t.Parallel()

	s := walkFixture(t, "vylog_sample.vylog")
	assert.NotZero(t, s.rowCount, "vylog_sample.vylog should have rows; got 0")
	// Try decoding one body as a VyLogBody.
	data := testutil.Load(t, "vylog_sample.vylog")

	off := 0
	for ; off+1 < len(data); off++ {
		if data[off] == '\n' && data[off+1] == '\n' {
			off += 2

			break
		}
	}

	slices, _, _, err := format.DecodeTxBlock(data[off:])
	require.NoError(t, err, "DecodeTxBlock")
	require.NotEmpty(t, slices, "no rows in first vylog tx block")
	row, _, err := format.DecodeXRow(slices[0])
	require.NoError(t, err, "DecodeXRow")
	require.NotEmptyf(t, row.BodyRaw, "vylog row body empty: row=%+v", row)
	body, err := format.DecodeVyLogBody(row.BodyRaw)
	require.NoError(t, err, "DecodeVyLogBody")
	t.Logf("vylog row 0: type=%d keys=%d", body.Type, len(body.Keys))
}
