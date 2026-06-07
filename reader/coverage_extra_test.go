package reader_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// badMetaImage returns a journal image whose meta header is truncated (no
// blank-line terminator), so os.Open / fs.Open / mmap all succeed but the
// subsequent meta decode fails. This drives the "open succeeded, construct
// failed → close the file we acquired" error branches of Open / OpenFS /
// OpenAt / OpenMmap.
func badMetaImage(t *testing.T) []byte {
	t.Helper()

	data := buildLog(t, 1)

	end := bytes.Index(data, []byte("\n\n"))
	require.GreaterOrEqual(t, end, 0)

	return data[:end] // No blank line → DecodeMeta fails.
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

// --- Open / OpenFS / OpenAt / OpenMmap: open-ok-but-decode-fails branch ---

func TestOpen_DecodeFailsAfterOpen(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "bad.xlog", badMetaImage(t))

	_, err := reader.Open(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, format.ErrMetaTruncated)
}

func TestOpen_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := reader.Open(filepath.Join(t.TempDir(), "nope.xlog"))
	require.Error(t, err)
}

func TestOpenFS_DecodeFailsAfterOpen(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"bad.xlog": {Data: badMetaImage(t)}}

	_, err := reader.OpenFS(fsys, "bad.xlog")
	require.Error(t, err)
	assert.ErrorIs(t, err, format.ErrMetaTruncated)
}

func TestReadHeaderFS_Missing(t *testing.T) {
	t.Parallel()

	_, err := reader.ReadHeaderFS(fstest.MapFS{}, "nope.xlog")
	require.Error(t, err)
}

func TestOpenAt_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := reader.OpenAt(filepath.Join(t.TempDir(), "nope.xlog"), 0)
	require.Error(t, err)
}

func TestOpenAt_DecodeFailsAfterOpen(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "bad.xlog", badMetaImage(t))

	_, err := reader.OpenAt(path, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, format.ErrMetaTruncated)
}

// TestOpenAt_ResumeFromBlockOffset resumes a real file at a non-zero block
// offset captured from a prior pass, exercising the seek + rebuilt-buffer path.
func TestOpenAt_ResumeFromBlockOffset(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "resume.xlog")
	writeFinalXlog(t, path, []int64{1, 2, 3, 4})

	r, err := reader.Open(path)
	require.NoError(t, err)

	// Read two rows, then capture the resume offset (a clean block boundary).
	_, err = r.Next()
	require.NoError(t, err)
	_, err = r.Next()
	require.NoError(t, err)

	off := r.Offset()
	require.Positive(t, off)
	require.NoError(t, r.Close())

	r2, err := reader.OpenAt(path, off)
	require.NoError(t, err)

	// Offset advances at drain time, so after reading rows 1 and 2 it points at
	// the start of block 2 (at-least-once resume re-reads row 2).
	require.Equal(t, []int64{2, 3, 4}, collectLSNs(t, r2))
	require.NoError(t, r2.Close())
}

func TestOpenMmap_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := reader.OpenMmap(filepath.Join(t.TempDir(), "nope.xlog"))
	require.Error(t, err)
}

func TestOpenMmap_DecodeFailsAfterMap(t *testing.T) {
	t.Parallel()

	// A non-empty file (so mmap is actually attempted) whose meta is malformed.
	path := writeTemp(t, "bad.xlog", badMetaImage(t))

	_, err := reader.OpenMmap(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, format.ErrMetaTruncated)
}

// TestOpenMmap_RowsAndClose exercises the mmap happy path end-to-end plus an
// explicit Close (munmap + fd close), and confirms a second Close is a no-op.
func TestOpenMmap_RowsAndClose(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "ok.xlog", buildMixedLog(t))

	r, err := reader.OpenMmap(path)
	require.NoError(t, err)

	n := 0
	for _, err := range r.Rows() {
		require.NoError(t, err)

		n++
	}

	assert.Positive(t, n)
	require.NoError(t, r.Close(), "munmap+close")
	require.NoError(t, r.Close(), "second Close is a no-op")
}

// --- streaming NextBlockRaw: corruption / resync / garbage paths ---

// craftGarbageBetweenBlocks returns a finalised xlog with raw garbage bytes
// inserted between the first and second valid blocks. Under SkipCorruptTx the
// reader's scanForwardToMagic must step over the garbage and resume.
func craftGarbageBetweenBlocks(t *testing.T) []byte {
	t.Helper()

	data := buildLog(t, 3)

	// Find the second block's magic (skip the first one).
	first := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, first, 0)

	second := bytes.Index(data[first+format.MarkerSize:], format.RowMarker[:])
	require.GreaterOrEqual(t, second, 0)
	insertAt := first + format.MarkerSize + second

	garbage := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

	out := make([]byte, 0, len(data)+len(garbage))
	out = append(out, data[:insertAt]...)
	out = append(out, garbage...)
	out = append(out, data[insertAt:]...)

	return out
}

func TestNextBlockRaw_UnknownMagic_Strict(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	// First block is fine.
	_, err = r.NextBlockRaw()
	require.NoError(t, err)

	// The garbage prefix is unknown magic — strict mode surfaces it.
	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrUnknownMagic)
}

func TestNextBlockRaw_GarbageSkipped(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)

	// Streaming reader: scanForwardToMagic steps across the garbage.
	r, err := reader.NewReader(bytes.NewReader(data), reader.SkipCorruptTx())
	require.NoError(t, err)

	blocks := 0

	for {
		_, err := r.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		blocks++
	}

	assert.Equal(t, 3, blocks, "all three valid blocks survive the garbage skip")
}

func TestNextBlockRaw_CorruptCRC_Strict(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF // flip a payload byte → CRC mismatch

	r, err := reader.NewReader(bytes.NewReader(corrupt))
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrCorruptCRC)
}

func TestNextBlockRaw_CorruptCRC_Skip(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF

	// Streaming reader under SkipCorruptTx: the corrupt block resyncs forward.
	r, err := reader.NewReader(bytes.NewReader(corrupt), reader.SkipCorruptTx())
	require.NoError(t, err)

	blocks := 0

	for {
		_, err := r.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		blocks++
	}

	assert.Less(t, blocks, 3, "the corrupt block is dropped")
	assert.Positive(t, blocks, "surviving blocks recovered")
}

func TestNextBlockRaw_TruncatedFixheader(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	// Cut in the middle of the first block's fixheader.
	truncated := data[:off+format.FixheaderSize-2]

	r, err := reader.NewReader(bytes.NewReader(truncated))
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

func TestNextBlockRaw_TruncatedPayload(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	// Keep the whole fixheader but chop the payload short.
	truncated := data[:off+format.FixheaderSize+1]

	r, err := reader.NewReader(bytes.NewReader(truncated))
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

func TestNextBlockRaw_CompressedBlock(t *testing.T) {
	t.Parallel()

	// buildMixedLog ends with a large ZRow (compressed) block.
	data := buildMixedLog(t)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	sawZRow := false

	for {
		block, err := r.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		var magic [4]byte

		copy(magic[:], block[:format.MarkerSize])
		if magic == format.ZRowMarker {
			sawZRow = true
		}
	}

	assert.True(t, sawZRow, "a compressed (ZRow) block must be returned verbatim")
}

// --- in-memory NextBlockRaw (nextBlockRawBytes / readBlockRawBytes) ---

func TestNextBlockRawBytes_UnknownMagic_Strict(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)

	r, err := reader.NewReaderBytes(data)
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrUnknownMagic)
}

func TestNextBlockRawBytes_GarbageSkipped(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)

	r, err := reader.NewReaderBytes(data, reader.SkipCorruptTx())
	require.NoError(t, err)

	blocks := 0

	for {
		_, err := r.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		blocks++
	}

	assert.Equal(t, 3, blocks)
}

func TestNextBlockRawBytes_CorruptCRC_StrictAndSkip(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF

	// Strict: CRC error surfaces.
	rs, err := reader.NewReaderBytes(corrupt)
	require.NoError(t, err)

	_, err = rs.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrCorruptCRC)

	// Skip: corrupt block dropped, the rest recovered.
	rk, err := reader.NewReaderBytes(corrupt, reader.SkipCorruptTx())
	require.NoError(t, err)

	blocks := 0

	for {
		_, err := rk.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		blocks++
	}

	assert.Less(t, blocks, 3)
	assert.Positive(t, blocks)
}

func TestNextBlockRawBytes_TruncatedFixheader(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	truncated := data[:off+format.FixheaderSize-2]

	r, err := reader.NewReaderBytes(truncated)
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

func TestNextBlockRawBytes_TruncatedPayload(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	truncated := data[:off+format.FixheaderSize+1]

	r, err := reader.NewReaderBytes(truncated)
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

func TestNextBlockRawBytes_TruncatedNoMarker_IgnoreMissingEOF(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	truncated := data[:len(data)-format.MarkerSize] // drop EOF marker

	// Strict: ErrTruncated at the partial/absent marker.
	rs, err := reader.NewReaderBytes(truncated)
	require.NoError(t, err)

	for range 2 {
		_, berr := rs.NextBlockRaw()
		require.NoError(t, berr)
	}

	_, err = rs.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTruncated)

	// Lenient: clean io.EOF.
	rl, err := reader.NewReaderBytes(truncated, reader.IgnoreMissingEOF())
	require.NoError(t, err)

	for range 2 {
		_, berr := rl.NextBlockRaw()
		require.NoError(t, berr)
	}

	_, err = rl.NextBlockRaw()
	require.ErrorIs(t, err, io.EOF)
}

func TestNextBlockRawBytes_PartialMagicTail(t *testing.T) {
	t.Parallel()

	// Append 2 stray bytes after the EOF marker is removed so fewer than 4
	// bytes remain but pos != len(buf): the "partial bytes before EOF" branch.
	data := buildLog(t, 1)
	trimmed := data[:len(data)-format.MarkerSize]
	withTail := append(bytes.Clone(trimmed), 0xAB, 0xCD)

	r, err := reader.NewReaderBytes(withTail)
	require.NoError(t, err)

	_, err = r.NextBlockRaw() // the one good block
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

// --- ZRow row-decoding via in-memory cursor (readTxBlockBytes ZRow branch) ---

func TestNewReaderBytes_ZRowRowDecode(t *testing.T) {
	t.Parallel()

	data := buildMixedLog(t) // contains a ZRow block as its final tx

	r, err := reader.NewReaderBytes(data)
	require.NoError(t, err)

	rows, drainErr := drainAll(t, r)
	require.ErrorIs(t, drainErr, io.EOF)
	require.NotEmpty(t, rows)

	// The last row carries the large (4 KiB) body that forced compression.
	last := rows[len(rows)-1]
	assert.Greater(t, len(last.BodyRaw), 4000, "the big ZRow tx body decompressed")
}

// --- streaming truncation inside a tx payload (readTxBlock short-payload) ---

func TestNext_TruncatedTxPayload_Streaming(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	// Keep the full first fixheader, chop its payload short.
	truncated := data[:off+format.FixheaderSize+1]

	r, err := reader.NewReader(bytes.NewReader(truncated))
	require.NoError(t, err)

	_, err = r.Next()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

// --- AcceptV012: a file whose version line is patched to 0.12, mmapped ---

func TestOpenMmap_AcceptV012(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)

	munged := bytes.Replace(data,
		[]byte("\n"+format.FormatVersion+"\n"),
		[]byte("\n0.12\n"), 1)
	require.NotEqual(t, data, munged, "version munge failed")

	path := writeTemp(t, "v012.xlog", munged)

	// Strict mmap rejects it.
	_, err := reader.OpenMmap(path)
	require.Error(t, err)

	// With AcceptV012 it opens and reads.
	r, err := reader.OpenMmap(path, reader.AcceptV012())
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Equal(t, format.LegacyFormatVersion, r.Meta().FormatVer)

	n := 0
	for _, err := range r.Rows() {
		require.NoError(t, err)

		n++
	}

	assert.Equal(t, 2, n)
}

// --- garbage-before-first-block under streaming loadNextTx resync ---

// TestNext_GarbageThenBlocks_Skip places garbage between blocks and drains rows
// via the streaming row cursor under SkipCorruptTx, exercising the streaming
// scanForwardToMagic + loadNextTx resync loop (distinct from the raw cursor).
func TestNext_GarbageThenBlocks_Skip(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)

	r, err := reader.NewReader(bytes.NewReader(data), reader.SkipCorruptTx())
	require.NoError(t, err)

	rows := 0

	for _, err := range r.Rows() {
		require.NoError(t, err)

		rows++
	}

	assert.Equal(t, 3, rows, "all three rows survive the garbage skip")
}

// TestNext_UnknownMagic_Strict drives the streaming loadNextTx default branch
// (unknown magic, strict) for the row cursor.
func TestNext_UnknownMagic_Strict(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	// First row decodes from block 1; the garbage then surfaces unknown magic.
	var lastErr error

	for _, err := range r.Rows() {
		if err != nil {
			lastErr = err

			break
		}
	}

	require.ErrorIs(t, lastErr, reader.ErrUnknownMagic)
}

// craftCompressedThreeTx writes three reasonably large txs so the writer emits
// compressed (ZRow) blocks, used to exercise the streaming ZRow raw path under
// resync as well.
func craftCompressedThreeTx(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, &format.Meta{
		Filetype: format.FiletypeXLOG,
		Version:  "go-xlog/test",
		VClock:   format.VClock{1: 0},
	})
	require.NoError(t, err)

	for i := range 3 {
		big := make([]byte, 0, 5000)
		big = append(big, 0xc5, 0x13, 0x88) // bin16 of 5000 bytes
		big = append(big, bytes.Repeat([]byte{byte(i)}, 5000)...)
		require.NoError(t, w.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, LSN: int64(i + 1), BodyRaw: big},
		}))
	}

	require.NoError(t, w.Close())

	return buf.Bytes()
}

func TestNextBlockRaw_CompressedStreaming(t *testing.T) {
	t.Parallel()

	data := craftCompressedThreeTx(t)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	zrows := 0

	for {
		block, err := r.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		var magic [4]byte

		copy(magic[:], block[:format.MarkerSize])
		if magic == format.ZRowMarker {
			zrows++
		}
	}

	assert.Equal(t, 3, zrows, "all three large txs compressed to ZRow blocks")
}

// --- in-memory ROW cursor over garbage / corrupt / truncated images ---
// (loadNextTxBytes / readTxBlockBytes / scanForwardToMagicBytes branches that
// the raw cursor does not touch)

func TestNextBytes_GarbageThenBlocks_Skip(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)

	r, err := reader.NewReaderBytes(data, reader.SkipCorruptTx())
	require.NoError(t, err)

	rows := 0

	for _, err := range r.Rows() {
		require.NoError(t, err)

		rows++
	}

	assert.Equal(t, 3, rows)
}

func TestNextBytes_UnknownMagic_Strict(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)

	r, err := reader.NewReaderBytes(data)
	require.NoError(t, err)

	var lastErr error

	for _, err := range r.Rows() {
		if err != nil {
			lastErr = err

			break
		}
	}

	require.ErrorIs(t, lastErr, reader.ErrUnknownMagic)
}

func TestNextBytes_CorruptCRC_Skip(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF

	r, err := reader.NewReaderBytes(corrupt, reader.SkipCorruptTx())
	require.NoError(t, err)

	rows, drainErr := drainAll(t, r)
	require.ErrorIs(t, drainErr, io.EOF)
	assert.Less(t, len(rows), 3)
}

func TestNextBytes_TruncatedFixheader(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	truncated := data[:off+format.FixheaderSize-2]

	r, err := reader.NewReaderBytes(truncated)
	require.NoError(t, err)

	_, err = r.Next()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

func TestNextBytes_TruncatedPayload(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	truncated := data[:off+format.FixheaderSize+1]

	r, err := reader.NewReaderBytes(truncated)
	require.NoError(t, err)

	_, err = r.Next()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

// TestNextBytes_PartialMagicTail exercises the "fewer than 4 bytes remain but
// not at an exact boundary" branch of the in-memory row cursor (loadNextTxBytes).
func TestNextBytes_PartialMagicTail(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 1)
	trimmed := data[:len(data)-format.MarkerSize]
	withTail := append(bytes.Clone(trimmed), 0xAB, 0xCD)

	r, err := reader.NewReaderBytes(withTail)
	require.NoError(t, err)

	_, drainErr := drainAll(t, r)
	require.ErrorIs(t, drainErr, reader.ErrTruncated)
}

// --- trailing garbage with no valid magic: scanForwardToMagic hits EOF ---

// craftTrailingGarbage returns a finalised xlog whose EOF marker (and tail) is
// replaced by garbage that contains no valid magic, so a SkipCorruptTx resync
// scans to EOF.
func craftTrailingGarbage(t *testing.T) []byte {
	t.Helper()

	data := buildLog(t, 2)
	// Replace the final EOF marker with non-magic bytes so the resync scan
	// runs off the end without finding a marker.
	out := bytes.Clone(data[:len(data)-format.MarkerSize])
	// Append a corrupt block-ish prefix that is unknown magic, then plain
	// garbage, so the row cursor enters resync and scans to EOF.
	out = append(out, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88)

	return out
}

func TestNext_TrailingGarbage_SkipToEOF_Streaming(t *testing.T) {
	t.Parallel()

	data := craftTrailingGarbage(t)

	// SkipCorruptTx resync runs off the end of the file: the trailing garbage
	// has no valid magic, so scanForwardToMagic exhausts the stream. The partial
	// bytes left over surface as truncation (a partial tail is never a clean EOF,
	// even under IgnoreMissingEOF — the marker is exactly 4 bytes).
	r, err := reader.NewReader(bytes.NewReader(data), reader.SkipCorruptTx())
	require.NoError(t, err)

	_, drainErr := drainAll(t, r)
	require.ErrorIs(t, drainErr, reader.ErrTruncated)

	r2, err := reader.NewReader(bytes.NewReader(data),
		reader.SkipCorruptTx(), reader.IgnoreMissingEOF())
	require.NoError(t, err)

	_, drainErr2 := drainAll(t, r2)
	require.ErrorIs(t, drainErr2, reader.ErrTruncated)
}

func TestNext_TrailingGarbage_SkipToEOF_Bytes(t *testing.T) {
	t.Parallel()

	data := craftTrailingGarbage(t)

	r, err := reader.NewReaderBytes(data, reader.SkipCorruptTx())
	require.NoError(t, err)

	_, drainErr := drainAll(t, r)
	require.ErrorIs(t, drainErr, reader.ErrTruncated)

	r2, err := reader.NewReaderBytes(data,
		reader.SkipCorruptTx(), reader.IgnoreMissingEOF())
	require.NoError(t, err)

	_, drainErr2 := drainAll(t, r2)
	require.ErrorIs(t, drainErr2, reader.ErrTruncated)
}

// TestNextBlockRaw_TrailingGarbage_SkipToEOF exercises the raw cursor's resync
// scanning off the end for both reader modes.
func TestNextBlockRaw_TrailingGarbage_SkipToEOF(t *testing.T) {
	t.Parallel()

	data := craftTrailingGarbage(t)

	// Streaming raw cursor.
	rs, err := reader.NewReader(bytes.NewReader(data), reader.SkipCorruptTx())
	require.NoError(t, err)

	var lastErr error

	for {
		_, e := rs.NextBlockRaw()
		if e != nil {
			lastErr = e

			break
		}
	}

	require.ErrorIs(t, lastErr, reader.ErrTruncated)

	// In-memory raw cursor.
	rb, err := reader.NewReaderBytes(data, reader.SkipCorruptTx())
	require.NoError(t, err)

	for {
		_, e := rb.NextBlockRaw()
		if e != nil {
			lastErr = e

			break
		}
	}

	require.ErrorIs(t, lastErr, reader.ErrTruncated)
}

// --- streaming partial-magic-before-EOF (peekMagic partial branch) ---

func TestNext_PartialMagicTail_Streaming(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 1)
	trimmed := data[:len(data)-format.MarkerSize]
	withTail := append(bytes.Clone(trimmed), 0xAB, 0xCD) // 2 stray bytes

	r, err := reader.NewReader(bytes.NewReader(withTail))
	require.NoError(t, err)

	_, drainErr := drainAll(t, r)
	require.ErrorIs(t, drainErr, reader.ErrTruncated)
}

func TestNextBlockRaw_PartialMagicTail_Streaming(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 1)
	trimmed := data[:len(data)-format.MarkerSize]
	withTail := append(bytes.Clone(trimmed), 0xAB, 0xCD)

	r, err := reader.NewReader(bytes.NewReader(withTail))
	require.NoError(t, err)

	_, err = r.NextBlockRaw() // the one good block
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTruncated)
}

// TestOpenAt_WithOption exercises the option-application loop in newReaderAt
// (the OpenAt construction path that walks opts).
func TestOpenAt_WithOption(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "opt.xlog")
	writeFinalXlog(t, path, []int64{1, 2})

	// Drop the EOF marker on disk so IgnoreMissingEOF is actually meaningful.
	raw, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw[:len(raw)-format.MarkerSize], 0o600))

	r, err := reader.OpenAt(path, 0, reader.IgnoreMissingEOF())
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	require.Equal(t, []int64{1, 2}, collectLSNs(t, r))
}

// --- ZRow block with a valid CRC over a corrupt compressed payload ---

// craftCorruptZRow builds a journal image whose single ZRow block passes its
// CRC check (recomputed over the mangled compressed bytes) but fails zstd
// decompression, driving the decompress-error branch.
func craftCorruptZRow(t *testing.T) []byte {
	t.Helper()

	// A body large enough that the writer compresses the tx into a ZRow block.
	big := make([]byte, 0, 5000)
	big = append(big, 0xc5, 0x13, 0x88) // bin16 of 5000 bytes
	big = append(big, bytes.Repeat([]byte{0x5A}, 5000)...)

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, &format.Meta{
		Filetype: format.FiletypeXLOG,
		Version:  "go-xlog/test",
		VClock:   format.VClock{1: 0},
	})
	require.NoError(t, err)
	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: big},
	}))
	require.NoError(t, w.Close())

	data := buf.Bytes()

	zoff := bytes.Index(data, format.ZRowMarker[:])
	require.GreaterOrEqual(t, zoff, 0, "writer must emit a ZRow block for the large tx")

	var fhBytes [format.FixheaderSize]byte

	copy(fhBytes[:], data[zoff:zoff+format.FixheaderSize])
	fh, err := format.DecodeFixheader(fhBytes)
	require.NoError(t, err)

	payloadStart := zoff + format.FixheaderSize
	payloadEnd := payloadStart + int(fh.Len)

	out := bytes.Clone(data)
	// Mangle the compressed payload so zstd decompression fails, then re-stamp the
	// CRC over the mangled bytes so the CRC check passes and we reach the
	// decompressor.
	for i := payloadStart; i < payloadEnd; i++ {
		out[i] ^= 0xFF
	}

	fh.CRC32C = format.CRC32C(out[payloadStart:payloadEnd])
	format.EncodeFixheader(&fhBytes, fh)
	copy(out[zoff:zoff+format.FixheaderSize], fhBytes[:])

	return out
}

func TestNext_DecompressError(t *testing.T) {
	t.Parallel()

	data := craftCorruptZRow(t)

	// In-memory row cursor (readTxBlockBytes ZRow decompress).
	rb, err := reader.NewReaderBytes(data)
	require.NoError(t, err)

	_, err = rb.Next()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decompress")

	// Streaming row cursor (readTxBlock ZRow decompress).
	rs, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	_, err = rs.Next()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decompress")
}

// --- error-injecting io.ReadSeeker: covers the non-EOF read-error branches ---

// errAfterReader is an io.ReadSeeker that serves data verbatim up to failAt
// bytes, then returns a non-EOF error. It drives the "read failed for a reason
// other than EOF" branches in peekMagic / readTxBlock / readBlockRaw /
// scanForwardToMagic that a plain byte slice (which only ever yields io.EOF)
// cannot reach.
type errAfterReader struct {
	data   []byte
	pos    int
	failAt int
	err    error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if e.pos >= e.failAt {
		return 0, e.err
	}

	n := copy(p, e.data[e.pos:min(e.failAt, len(e.data))])
	e.pos += n

	return n, nil
}

func (e *errAfterReader) Seek(offset int64, whence int) (int64, error) {
	// NewReader never seeks in the current implementation; satisfy the interface.
	switch whence {
	case io.SeekStart:
		e.pos = int(offset)
	case io.SeekCurrent:
		e.pos += int(offset)
	case io.SeekEnd:
		e.pos = len(e.data) + int(offset)
	}

	return int64(e.pos), nil
}

var errInjected = errInjectedType{}

type errInjectedType struct{}

func (errInjectedType) Error() string { return "injected read failure" }

// metaLen returns the byte length of the meta header (offset of the first block).
func metaLen(t *testing.T, data []byte) int {
	t.Helper()

	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	return off
}

// TestNext_ReadError_DuringPeekMagic injects a non-EOF error right at the first
// block boundary, so peekMagic's catch-all error branch fires.
func TestNext_ReadError_DuringPeekMagic(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	src := &errAfterReader{data: data, failAt: metaLen(t, data), err: errInjected}

	r, err := reader.NewReader(src)
	require.NoError(t, err)

	_, err = r.Next()
	require.ErrorIs(t, err, errInjected)
}

func TestNextBlockRaw_ReadError_DuringPeekMagic(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	src := &errAfterReader{data: data, failAt: metaLen(t, data), err: errInjected}

	r, err := reader.NewReader(src)
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, errInjected)
}

// TestNext_ReadError_DuringPayload injects a non-EOF error partway into the
// first block's payload (past its fixheader), so readTxBlock's payload-read
// catch-all branch fires.
func TestNext_ReadError_DuringPayload(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	// Fail a couple of bytes into the first payload (after the 4-byte magic
	// peek and the 19-byte fixheader have been served).
	src := &errAfterReader{
		data:   data,
		failAt: metaLen(t, data) + format.FixheaderSize + 1,
		err:    errInjected,
	}

	r, err := reader.NewReader(src)
	require.NoError(t, err)

	_, err = r.Next()
	require.ErrorIs(t, err, errInjected)
}

func TestNextBlockRaw_ReadError_DuringPayload(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	src := &errAfterReader{
		data:   data,
		failAt: metaLen(t, data) + format.FixheaderSize + 1,
		err:    errInjected,
	}

	r, err := reader.NewReader(src)
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, errInjected)
}

// --- Scan / ScanTx / Txs error propagation paths ---

// TestScan_PropagatesCorruptError drives scanRow's decode/load error branch
// (scanErr set, Scan returns false) over a corrupt image.
func TestScan_PropagatesCorruptError(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF

	r, err := reader.NewReaderBytes(corrupt)
	require.NoError(t, err)

	for r.Scan() { //nolint:revive // drain until the error stops it
	}

	require.ErrorIs(t, r.Err(), reader.ErrCorruptCRC)
}

// TestScanTx_PropagatesCorruptError drives ScanTx's scanRow-failure branch
// (drop the half-filled slot, surface the error).
func TestScanTx_PropagatesCorruptError(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF

	r, err := reader.NewReaderBytes(corrupt)
	require.NoError(t, err)

	for r.ScanTx() { //nolint:revive // drain
	}

	require.ErrorIs(t, r.Err(), reader.ErrCorruptCRC)
}

// TestScanTx_TruncatedMidTx drives the "rows accumulated but stream ended before
// IsCommit" branch of ScanTx (ErrTruncated synthesized).
func TestScanTx_TruncatedMidTx(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, &format.Meta{
		Filetype: format.FiletypeXLOG,
		Version:  "go-xlog/test",
		VClock:   format.VClock{1: 0},
	})
	require.NoError(t, err)

	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: []byte{0xc4, 1, 0xaa}},
		{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: []byte{0xc4, 1, 0xbb}},
		{Type: iproto.IPROTO_INSERT, LSN: 3, BodyRaw: []byte{0xc4, 1, 0xcc}},
	}))
	require.NoError(t, w.Close())

	data := buf.Bytes()
	// Drop the EOF marker so the single 3-row tx block is the whole stream but
	// no commit terminator follows after a truncation. To truncate mid-tx we cut
	// inside the payload: keep the fixheader, chop the payload short.
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	truncated := data[:off+format.FixheaderSize+1]

	r, err := reader.NewReaderBytes(truncated)
	require.NoError(t, err)

	require.False(t, r.ScanTx())
	require.ErrorIs(t, r.Err(), reader.ErrTruncated)
}

// TestTxs_YieldsErrorThenStops drives the Txs iterator's error-yield-and-stop
// path and the early-break consumer path.
func TestTxs_YieldsErrorThenStops(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF

	r, err := reader.NewReaderBytes(corrupt)
	require.NoError(t, err)

	var gotErr error

	for _, err := range r.Txs() {
		if err != nil {
			gotErr = err

			break
		}
	}

	require.ErrorIs(t, gotErr, reader.ErrCorruptCRC)
}

// TestRows_EarlyBreak drives the Rows iterator's "consumer returned false" path.
func TestRows_EarlyBreak(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 5)

	r, err := reader.NewReaderBytes(data)
	require.NoError(t, err)

	seen := 0

	for range r.Rows() {
		seen++
		if seen == 2 {
			break // exercise the !yield early-stop branch
		}
	}

	assert.Equal(t, 2, seen)
}

// TestTxs_EarlyBreak drives the Txs iterator's "consumer returned false" path.
func TestTxs_EarlyBreak(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 5)

	r, err := reader.NewReaderBytes(data)
	require.NoError(t, err)

	seen := 0

	for range r.Txs() {
		seen++
		if seen == 2 {
			break
		}
	}

	assert.Equal(t, 2, seen)
}

// --- crafted fixheaders: MaxTxPayloadLen and fixheader-decode-shape errors ---

// metaPrefix extracts the meta-header bytes (everything before the first block).
func metaPrefix(t *testing.T) []byte {
	t.Helper()

	data := buildLog(t, 1)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	return bytes.Clone(data[:off])
}

// TestTxTooLarge drives the MaxTxPayloadLen guard in both reader modes: a block
// whose fixheader declares a payload above the cap is rejected before any
// allocation.
func TestTxTooLarge(t *testing.T) {
	t.Parallel()

	var fh [format.FixheaderSize]byte

	format.EncodeFixheader(&fh, &format.Fixheader{
		Magic:  format.RowMarker,
		Len:    reader.MaxTxPayloadLen + 1,
		CRC32C: 0,
	})

	image := append(metaPrefix(t), fh[:]...)
	image = append(image, format.EOFMarker[:]...) // not reached

	// In-memory row cursor.
	rb, err := reader.NewReaderBytes(image)
	require.NoError(t, err)

	_, err = rb.Next()
	require.ErrorIs(t, err, reader.ErrTxTooLarge)

	// In-memory raw cursor.
	rbr, err := reader.NewReaderBytes(image)
	require.NoError(t, err)

	_, err = rbr.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTxTooLarge)

	// Streaming row cursor.
	rs, err := reader.NewReader(bytes.NewReader(image))
	require.NoError(t, err)

	_, err = rs.Next()
	require.ErrorIs(t, err, reader.ErrTxTooLarge)

	// Streaming raw cursor.
	rsr, err := reader.NewReader(bytes.NewReader(image))
	require.NoError(t, err)

	_, err = rsr.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTxTooLarge)
}

// TestFixheaderShapeError drives the DecodeFixheaderInto failure branch: a valid
// magic followed by a malformed length mp-uint (0xc1 is the never-used msgpack
// byte) makes the fixheader decode fail with a shape error.
func TestFixheaderShapeError(t *testing.T) {
	t.Parallel()

	var fh [format.FixheaderSize]byte

	format.EncodeFixheader(&fh, &format.Fixheader{
		Magic:  format.RowMarker,
		Len:    1,
		CRC32C: 0,
	})
	// Corrupt the byte right after the magic (the Len mp-uint prefix).
	fh[format.MarkerSize] = 0xc1

	image := append(metaPrefix(t), fh[:]...)
	image = append(image, 0x00)                   // a payload byte
	image = append(image, format.EOFMarker[:]...) // not reached

	rb, err := reader.NewReaderBytes(image)
	require.NoError(t, err)

	_, err = rb.Next()
	require.Error(t, err) // ErrFixheaderShape, wrapped

	rs, err := reader.NewReader(bytes.NewReader(image))
	require.NoError(t, err)

	_, err = rs.Next()
	require.Error(t, err)

	// Raw cursors hit the same decode in readBlockRaw / readBlockRawBytes.
	rbr, err := reader.NewReaderBytes(image)
	require.NoError(t, err)

	_, err = rbr.NextBlockRaw()
	require.Error(t, err)

	rsr, err := reader.NewReader(bytes.NewReader(image))
	require.NoError(t, err)

	_, err = rsr.NextBlockRaw()
	require.Error(t, err)
}

// --- streaming CRC-skip resync through readTxBlock (not just readBlockRaw) ---

func TestNext_CorruptCRC_Skip_Streaming(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF

	r, err := reader.NewReader(bytes.NewReader(corrupt), reader.SkipCorruptTx())
	require.NoError(t, err)

	rows := 0

	for _, err := range r.Rows() {
		require.NoError(t, err)

		rows++
	}

	assert.Less(t, rows, 3, "corrupt block dropped via readTxBlock resync")
	assert.Positive(t, rows)
}

// TestNext_ReadError_DuringFixheader injects a non-EOF error partway through the
// first block's fixheader read, so readTxBlock / readBlockRaw's fixheader-read
// catch-all (non-EOF) branch fires.
func TestNext_ReadError_DuringFixheader(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	src := &errAfterReader{
		data:   data,
		failAt: metaLen(t, data) + 2, // a couple of bytes into the fixheader
		err:    errInjected,
	}

	r, err := reader.NewReader(src)
	require.NoError(t, err)

	_, err = r.Next()
	require.ErrorIs(t, err, errInjected)
}

func TestNextBlockRaw_ReadError_DuringFixheader(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)
	src := &errAfterReader{
		data:   data,
		failAt: metaLen(t, data) + 2,
		err:    errInjected,
	}

	r, err := reader.NewReader(src)
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, errInjected)
}

// TestNext_ReadError_DuringResyncScan injects a non-EOF error while the cursor
// is resyncing over garbage under SkipCorruptTx, so scanForwardToMagic's
// catch-all peek-error branch fires.
func TestNext_ReadError_DuringResyncScan(t *testing.T) {
	t.Parallel()

	data := craftGarbageBetweenBlocks(t)
	// The garbage starts right after the first block. Fail a few bytes into the
	// garbage region so the resync scan's Peek surfaces the injected error.
	first := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, first, 0)

	// Offset of the first block's payload end ≈ start of the garbage. Find the
	// garbage by locating the 0x01..0x08 sentinel.
	garbageAt := bytes.Index(data, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})
	require.Greater(t, garbageAt, first)

	// Serve all 8 garbage bytes (and the first block) so the FIRST resync peek
	// reads a full 4-byte unknown-magic window, the cursor enters
	// scanForwardToMagic, and a subsequent refill peek surfaces the injected
	// non-EOF error from inside the scan loop.
	src := &errAfterReader{data: data, failAt: garbageAt + format.MarkerSize, err: errInjected}

	r, err := reader.NewReader(src, reader.SkipCorruptTx())
	require.NoError(t, err)

	// First block's row decodes fine; the resync over the garbage then fails.
	_, _ = r.Next()

	_, drainErr := drainAll(t, r)
	require.ErrorIs(t, drainErr, errInjected)
}
