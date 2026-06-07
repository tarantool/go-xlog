package pipe_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/pipe"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// buildRawSource writes an in-memory xlog with three small single-row
// (uncompressed) txs followed by one big block whose payload crosses the
// compression threshold and therefore lands as a ZRow block on disk. Returns
// the raw bytes.
func buildRawSource(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, newMeta())
	require.NoError(t, err, "NewWriter")

	for lsn := int64(1); lsn <= 3; lsn++ {
		row := format.XRow{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: lsn, BodyRaw: dmlBody(uint64(lsn), 100)}
		require.NoError(t, w.WriteTx([]format.XRow{row}), "WriteTx single lsn=%d", lsn)
	}

	// A wide tuple of zeros compresses well and crosses CompressThreshold,
	// so this tx is emitted as a ZRow (compressed) block.
	big := make([]uint64, 4096)
	bigRow := format.XRow{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: 4, BodyRaw: dmlBody(big...)}
	require.NoError(t, w.WriteTx([]format.XRow{bigRow}), "WriteTx big")
	require.NoError(t, w.Close(), "Close")

	return buf.Bytes()
}

// readAllRows decodes every row from an in-memory xlog.
func readAllRows(t *testing.T, b []byte) []format.XRow {
	t.Helper()

	r, err := reader.NewReader(bytes.NewReader(b))
	require.NoError(t, err, "NewReader")

	var rows []format.XRow

	for {
		row, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return rows
			}

			require.NoError(t, err, "Next")
		}

		rows = append(rows, row)
	}
}

// containsMarker reports whether b contains the 4-byte marker anywhere.
func containsMarker(b []byte, marker [4]byte) bool {
	return bytes.Contains(b, marker[:])
}

// firstMarkerOffset returns the byte offset of the first occurrence of marker,
// or -1 if absent.
func firstMarkerOffset(b []byte, marker [4]byte) int {
	return bytes.Index(b, marker[:])
}

// TestCopyRaw_RoundTrip — CopyRaw reproduces every source row byte-identically.
func TestCopyRaw_RoundTrip(t *testing.T) {
	t.Parallel()

	src := buildRawSource(t)

	r, err := reader.NewReader(bytes.NewReader(src))
	require.NoError(t, err, "NewReader src")

	var dst bytes.Buffer

	w, err := writer.NewWriter(&dst, newMeta())
	require.NoError(t, err, "NewWriter dst")

	blocks, err := pipe.CopyRaw(r, w)
	require.NoError(t, err, "CopyRaw")
	require.NoError(t, w.Close(), "dst Close")

	// 3 single-row txs + 1 big tx = 4 physical blocks.
	assert.Equal(t, int64(4), blocks, "blocks copied")

	want := readAllRows(t, src)
	got := readAllRows(t, dst.Bytes())
	assert.Equal(t, want, got, "rows must round-trip byte-identically")
}

// TestCopyRaw_PreservesCompression — a ZRow source block stays a ZRow block on
// disk after CopyRaw (no decompress/recompress round trip).
func TestCopyRaw_PreservesCompression(t *testing.T) {
	t.Parallel()

	src := buildRawSource(t)
	require.True(t, containsMarker(src, format.ZRowMarker), "source must contain a ZRow block")

	r, err := reader.NewReader(bytes.NewReader(src))
	require.NoError(t, err, "NewReader src")

	var dst bytes.Buffer

	w, err := writer.NewWriter(&dst, newMeta())
	require.NoError(t, err, "NewWriter dst")

	_, err = pipe.CopyRaw(r, w)
	require.NoError(t, err, "CopyRaw")
	require.NoError(t, w.Close(), "dst Close")

	assert.True(t, containsMarker(dst.Bytes(), format.ZRowMarker), "dst must keep the ZRow block")
}

// TestCopyRaw_EmptySource — a source with no tx blocks (just meta + EOF) copies
// zero blocks and yields an empty, well-formed destination.
func TestCopyRaw_EmptySource(t *testing.T) {
	t.Parallel()

	var srcBuf bytes.Buffer

	sw, err := writer.NewWriter(&srcBuf, newMeta())
	require.NoError(t, err, "NewWriter src")
	require.NoError(t, sw.Close(), "src Close")

	r, err := reader.NewReader(bytes.NewReader(srcBuf.Bytes()))
	require.NoError(t, err, "NewReader src")

	var dst bytes.Buffer

	w, err := writer.NewWriter(&dst, newMeta())
	require.NoError(t, err, "NewWriter dst")

	blocks, err := pipe.CopyRaw(r, w)
	require.NoError(t, err, "CopyRaw")
	require.NoError(t, w.Close(), "dst Close")

	assert.Equal(t, int64(0), blocks, "no blocks")
	assert.Empty(t, readAllRows(t, dst.Bytes()), "dst has no rows")
}

// TestCopyRaw_CorruptNoSkip — a corrupt block surfaces ErrCorruptCRC when
// SkipCorruptTx is not set.
func TestCopyRaw_CorruptNoSkip(t *testing.T) {
	t.Parallel()

	src := buildRawSource(t)
	corruptFirstPayloadByte(t, src)

	r, err := reader.NewReader(bytes.NewReader(src))
	require.NoError(t, err, "NewReader src")

	var dst bytes.Buffer

	w, err := writer.NewWriter(&dst, newMeta())
	require.NoError(t, err, "NewWriter dst")

	_, err = pipe.CopyRaw(r, w)
	require.Error(t, err, "CopyRaw must fail on corruption")
	assert.ErrorIs(t, err, reader.ErrCorruptCRC, "want ErrCorruptCRC")
}

// TestCopyRaw_CorruptSkip — with SkipCorruptTx the corrupt first block is
// skipped and the remaining blocks copy cleanly.
func TestCopyRaw_CorruptSkip(t *testing.T) {
	t.Parallel()

	src := buildRawSource(t)
	corruptFirstPayloadByte(t, src)

	r, err := reader.NewReader(bytes.NewReader(src), reader.SkipCorruptTx())
	require.NoError(t, err, "NewReader src")

	var dst bytes.Buffer

	w, err := writer.NewWriter(&dst, newMeta())
	require.NoError(t, err, "NewWriter dst")

	blocks, err := pipe.CopyRaw(r, w)
	require.NoError(t, err, "CopyRaw with skip")
	require.NoError(t, w.Close(), "dst Close")

	// First (LSN=1) block dropped; the other 3 blocks survive.
	assert.Equal(t, int64(3), blocks, "blocks copied after skipping the corrupt one")

	got := readAllRows(t, dst.Bytes())
	require.Len(t, got, 3, "three rows survive")
	assert.Equal(t, int64(2), got[0].LSN, "first surviving row is LSN=2")
}

// TestCopyRaw_NilGuards — nil src / dst are rejected.
func TestCopyRaw_NilGuards(t *testing.T) {
	t.Parallel()

	_, err := pipe.CopyRaw(nil, nil)
	require.ErrorIs(t, err, pipe.ErrNilSrcReader, "nil src")

	r, err := reader.NewReader(bytes.NewReader(buildRawSource(t)))
	require.NoError(t, err, "NewReader")

	_, err = pipe.CopyRaw(r, nil)
	require.ErrorIs(t, err, pipe.ErrNilDstWriter, "nil dst")
}

// corruptFirstPayloadByte flips the first payload byte of the first tx block
// in b (the byte just past the first RowMarker's fixheader), breaking its CRC.
func corruptFirstPayloadByte(t *testing.T, b []byte) {
	t.Helper()

	off := firstMarkerOffset(b, format.RowMarker)
	require.GreaterOrEqual(t, off, 0, "source must contain a RowMarker")

	payload := off + format.FixheaderSize
	require.Less(t, payload, len(b), "payload byte in range")

	b[payload] ^= 0xFF
}
