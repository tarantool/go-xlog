package reader_test

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

var openAtInstance = uuid.MustParse("00000000-0000-0000-0000-0000000000aa")

// body is a tiny valid msgpack body map {0x10: lsn}; valid for lsn <= 127.
func body(lsn int64) []byte { return []byte{0x81, 0x10, byte(lsn)} }

func testMeta() *format.Meta {
	return &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "go-xlog/test",
		InstanceUUID: openAtInstance,
		VClock:       format.VClock{1: 0},
	}
}

// writeFinalXlog writes a finalised xlog at path with one single-row tx per lsn.
func writeFinalXlog(t *testing.T, path string, lsns []int64) {
	t.Helper()

	w, err := writer.Create(path, testMeta())
	require.NoError(t, err)

	for _, lsn := range lsns {
		require.NoError(t, w.WriteTx([]format.XRow{{
			Type:      iproto.IPROTO_INSERT,
			ReplicaID: 1,
			LSN:       lsn,
			BodyRaw:   body(lsn),
		}}))
	}

	require.NoError(t, w.Close())
}

func collectLSNs(t *testing.T, r *reader.Reader) []int64 {
	t.Helper()

	var got []int64

	for {
		row, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		got = append(got, row.LSN)
	}

	return got
}

func TestOffset_OpenAtSuffix(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.xlog")
	writeFinalXlog(t, path, []int64{1, 2, 3, 4, 5})

	// Read row by row, capturing the offset reported right after each row.
	r, err := reader.Open(path)
	require.NoError(t, err)

	var offAfter []int64 // offAfter[k] = Offset() after reading the (k+1)-th row

	for {
		_, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		offAfter = append(offAfter, r.Offset())
	}

	require.NoError(t, r.Close())
	require.Len(t, offAfter, 5)

	// Resuming at the offset captured after reading row k yields rows [k..5]
	// (at-least-once: row k is re-read because Offset is the start of its block).
	r3, err := reader.OpenAt(path, offAfter[2]) // after row 3 → start of block 3
	require.NoError(t, err)

	require.Equal(t, []int64{3, 4, 5}, collectLSNs(t, r3))
	require.NoError(t, r3.Close())
}

func TestOpenAt_ZeroEqualsOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.xlog")
	writeFinalXlog(t, path, []int64{1, 2, 3})

	r0, err := reader.OpenAt(path, 0)
	require.NoError(t, err)

	require.Equal(t, []int64{1, 2, 3}, collectLSNs(t, r0))
	require.NoError(t, r0.Close())
}

func TestOffset_PastEndYieldsNothing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.xlog")
	writeFinalXlog(t, path, []int64{1, 2, 3})

	r, err := reader.Open(path)
	require.NoError(t, err)

	require.Equal(t, []int64{1, 2, 3}, collectLSNs(t, r))

	end := r.Offset()
	require.NoError(t, r.Close())

	r2, err := reader.OpenAt(path, end)
	require.NoError(t, err)

	require.Empty(t, collectLSNs(t, r2))
	require.NoError(t, r2.Close())
}

func TestSawEOFMarker(t *testing.T) {
	t.Parallel()

	// Finalised file: marker present.
	path := filepath.Join(t.TempDir(), "final.xlog")
	writeFinalXlog(t, path, []int64{1, 2})

	r, err := reader.Open(path)
	require.NoError(t, err)

	require.Equal(t, []int64{1, 2}, collectLSNs(t, r))
	require.True(t, r.SawEOFMarker(), "finalised file should report the EOF marker")
	require.NoError(t, r.Close())

	// Unfinalised image (no marker), read with IgnoreMissingEOF.
	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, testMeta())
	require.NoError(t, err)

	require.NoError(t, w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: 1, BodyRaw: body(1)}}))
	require.NoError(t, w.Sync()) // flush block, but no Close → no marker

	r2, err := reader.NewReaderBytes(buf.Bytes(), reader.IgnoreMissingEOF())
	require.NoError(t, err)

	require.Equal(t, []int64{1}, collectLSNs(t, r2))
	require.False(t, r2.SawEOFMarker(), "unfinalised image should not report a marker")
}
