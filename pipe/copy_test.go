package pipe_test

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5/msgpcode"

	"github.com/tarantool/go-xlog/filter"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/pipe"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// --- helpers (mirrors writer/testhelpers_test.go but in this pkg) ---.

func appendMPUint(buf []byte, n uint64) []byte {
	switch {
	case n <= 0x7f:
		return append(buf, byte(n))
	case n <= 0xff:
		return append(buf, msgpcode.Uint8, byte(n))
	case n <= 0xffff:
		buf = append(buf, msgpcode.Uint16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	case n <= 0xffffffff:
		buf = append(buf, msgpcode.Uint32)

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(n))

		return append(buf, tmp[:]...)
	default:
		buf = append(buf, msgpcode.Uint64)

		var tmp [8]byte
		binary.BigEndian.PutUint64(tmp[:], n)

		return append(buf, tmp[:]...)
	}
}

func appendMPMapHeader(buf []byte, n int) []byte {
	if n <= 15 {
		return append(buf, msgpcode.FixedMapLow|byte(n))
	}

	buf = append(buf, msgpcode.Map16)

	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(n))

	return append(buf, tmp[:]...)
}

func appendMPArrayHeader(buf []byte, n int) []byte {
	if n <= 15 {
		return append(buf, msgpcode.FixedArrayLow|byte(n))
	}

	buf = append(buf, msgpcode.Array16)

	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(n))

	return append(buf, tmp[:]...)
}

func dmlBody(vals ...uint64) []byte {
	var b []byte

	b = appendMPMapHeader(b, 2)
	b = appendMPUint(b, 0x10) // KeySpaceID.
	b = appendMPUint(b, 512)  // spaceID.
	b = appendMPUint(b, 0x21) // KeyTuple.

	b = appendMPArrayHeader(b, len(vals))
	for _, v := range vals {
		b = appendMPUint(b, v)
	}

	return b
}

func newMeta() *format.Meta {
	return &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "go-xlog/test",
		InstanceUUID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		VClock:       format.VClock{1: 0},
	}
}

// buildSource writes 3 single-stmt txs (LSNs 1, 2, 3, all ReplicaID=1) plus
// one 3-row multistmt tx (LSNs 4, 5, 6, ReplicaID=1) to a fresh file.
func buildSource(t *testing.T, path string) {
	t.Helper()

	w, err := writer.Create(path, newMeta())
	require.NoError(t, err, "writer.Create")

	for lsn := int64(1); lsn <= 3; lsn++ {
		row := format.XRow{
			Type:      iproto.IPROTO_INSERT,
			ReplicaID: 1,
			LSN:       lsn,
			BodyRaw:   dmlBody(uint64(lsn), 100),
		}
		require.NoError(t, w.WriteTx([]format.XRow{row}), "WriteTx single lsn=%d", lsn)
	}
	// Multistmt: LSNs 4, 5, 6. Writer.assignTxIDs will set TSN=4 and put
	// FlagCommit on the last row.
	multi := []format.XRow{
		{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: 4, BodyRaw: dmlBody(4, 200)},
		{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: 5, BodyRaw: dmlBody(5, 200)},
		{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: 6, BodyRaw: dmlBody(6, 200)},
	}
	require.NoError(t, w.WriteTx(multi), "WriteTx multi")
	require.NoError(t, w.Close(), "Close")
}

// openSrcDst opens src for reading and creates a fresh dst writer in
// the same temp dir.
func openSrcDst(t *testing.T, srcPath, dstPath string) (*reader.Reader, *writer.Writer) {
	t.Helper()

	r, err := reader.Open(srcPath)
	require.NoError(t, err, "reader.Open(%q)", srcPath)
	w, err := writer.Create(dstPath, newMeta())
	require.NoError(t, err, "writer.Create(%q)", dstPath)

	return r, w
}

// countRows reads dst back and returns the total row count.
func countRows(t *testing.T, path string) int64 {
	t.Helper()

	r, err := reader.Open(path)
	require.NoError(t, err, "reader.Open(%q)", path)

	defer func() { _ = r.Close() }()

	var n int64

	for {
		_, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n
			}

			require.NoError(t, err, "Next")
		}

		n++
	}
}

// TestCopy_NoFilters — empty filter list keeps every row.
func TestCopy_NoFilters(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src.xlog")
	dst := filepath.Join(dir, "dst.xlog")

	buildSource(t, src)

	r, w := openSrcDst(t, src, dst)

	defer func() { _ = r.Close() }()

	n, err := pipe.Copy(r, w)
	require.NoError(t, err, "Copy")
	require.NoError(t, w.Close(), "dst Close")
	assert.Equal(t, int64(6), n, "rows written")
	assert.Equal(t, int64(6), countRows(t, dst), "dst row count")
}

// TestCopy_FromVClock — FromVClock({1: 2}) drops LSNs 1 and 2 (single-stmt
// txs of one row each are dropped) but keeps LSN=3 single-stmt and the
// 3-row multistmt tx (rows 4-6 all > 2). Total = 1 + 3 = 4 rows.
func TestCopy_FromVClock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src.xlog")
	dst := filepath.Join(dir, "dst.xlog")

	buildSource(t, src)

	r, w := openSrcDst(t, src, dst)

	defer func() { _ = r.Close() }()

	n, err := pipe.Copy(r, w, filter.FromVClock(format.VClock{1: 2}))
	require.NoError(t, err, "Copy")
	require.NoError(t, w.Close(), "dst Close")
	assert.Equal(t, int64(4), n, "rows written")
	assert.Equal(t, int64(4), countRows(t, dst), "dst row count")
}

// TestCopy_AnyRowMatches_KeepsWholeTx — LSNRange(1, 5, 5) matches only the
// middle row of the 3-row multistmt tx. Any-row-matches means
// the entire 3-row tx is written. None of the single-row txs match → 3
// rows total.
func TestCopy_AnyRowMatches_KeepsWholeTx(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src.xlog")
	dst := filepath.Join(dir, "dst.xlog")

	buildSource(t, src)

	r, w := openSrcDst(t, src, dst)

	defer func() { _ = r.Close() }()

	n, err := pipe.Copy(r, w, filter.LSNRange(1, 5, 5))
	require.NoError(t, err, "Copy")
	require.NoError(t, w.Close(), "dst Close")
	assert.Equal(t, int64(3), n, "rows written")
	assert.Equal(t, int64(3), countRows(t, dst), "dst row count")
}

// TestCopy_NothingMatches_EmptyDst — LSNRange that matches no rows yields
// an empty destination file (meta + EOF marker, no tx blocks). Reader.Open
// succeeds; Next returns io.EOF immediately.
func TestCopy_NothingMatches_EmptyDst(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src.xlog")
	dst := filepath.Join(dir, "dst.xlog")

	buildSource(t, src)

	r, w := openSrcDst(t, src, dst)

	defer func() { _ = r.Close() }()

	n, err := pipe.Copy(r, w, filter.LSNRange(1, 10, 20))
	require.NoError(t, err, "Copy")
	require.NoError(t, w.Close(), "dst Close")
	assert.Equal(t, int64(0), n, "rows written")

	// Confirm dst exists, opens cleanly, and yields no rows.
	_, err = os.Stat(dst)
	require.NoError(t, err, "dst should exist")
	assert.Equal(t, int64(0), countRows(t, dst), "dst row count")
}
