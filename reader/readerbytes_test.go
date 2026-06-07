package reader_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// drainAll reads every row from r via Next, returning the rows and the terminal
// error (io.EOF at a clean end). It clones each BodyRaw so retained rows stay
// valid: Next's BodyRaw aliases the reader's reused read buffer in the streaming
// mode (the in-memory mode aliases the stable backing buffer instead), so
// cloning makes the helper safe regardless of reader mode.
func drainAll(t *testing.T, r *reader.Reader) ([]format.XRow, error) {
	t.Helper()

	var rows []format.XRow

	for {
		row, err := r.Next()
		if err != nil {
			return rows, fmt.Errorf("drain: %w", err)
		}

		row.BodyRaw = bytes.Clone(row.BodyRaw)
		rows = append(rows, row)
	}
}

// TestNewReaderBytes_MatchesStream — the in-memory reader yields exactly the
// rows the streaming reader does over the same bytes.
func TestNewReaderBytes_MatchesStream(t *testing.T) {
	t.Parallel()

	data := buildMixedLog(t)

	rs, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	rm, err := reader.NewReaderBytes(data)
	require.NoError(t, err)

	want, wantErr := drainAll(t, rs)
	got, gotErr := drainAll(t, rm)

	require.ErrorIs(t, wantErr, io.EOF)
	require.ErrorIs(t, gotErr, io.EOF)
	require.Len(t, got, len(want), "row count")

	for i := range want {
		assert.Equal(t, want[i], got[i], "row %d", i)
	}
}

// TestNewReaderBytes_ZeroCopyAlias — with WithAliasBodies the row bodies alias
// the input buffer directly (no arena copy) and stay valid after the reader has
// advanced past them, because the buffer outlives the reader.
func TestNewReaderBytes_ZeroCopyAlias(t *testing.T) {
	t.Parallel()

	data := buildMixedLog(t)

	rm, err := reader.NewReaderBytes(data, reader.WithAliasBodies())
	require.NoError(t, err)

	// Retain every row (and its aliasing body) through the full drain.
	var rows []format.XRow

	for rm.Scan() {
		rows = append(rows, rm.Row())
	}

	require.NoError(t, rm.Err())
	require.NotEmpty(t, rows)

	// The first uncompressed row's body must alias the backing buffer, proving
	// zero-copy (the body bytes live inside data, not in a fresh allocation).
	first := rows[0]
	require.NotEmpty(t, first.BodyRaw)
	assert.True(t, aliasesBuffer(first.BodyRaw, data),
		"aliased body must point inside the input buffer")

	// Bodies retained from early rows are still intact after the full drain.
	ref, _ := drainAll2(t, data)
	require.Len(t, rows, len(ref))

	for i := range ref {
		assert.Equal(t, ref[i].BodyRaw, rows[i].BodyRaw, "retained body %d intact", i)
	}
}

// TestOpenMmap_MatchesOpen — OpenMmap reads a real file identically to Open.
func TestOpenMmap_MatchesOpen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "x.xlog")
	require.NoError(t, os.WriteFile(path, buildMixedLog(t), 0o600))

	ro, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = ro.Close() }()

	rm, err := reader.OpenMmap(path)
	require.NoError(t, err)

	defer func() { _ = rm.Close() }()

	want, wantErr := drainAll(t, ro)
	got, gotErr := drainAll(t, rm)

	require.ErrorIs(t, wantErr, io.EOF)
	require.ErrorIs(t, gotErr, io.EOF)
	require.Len(t, got, len(want))

	for i := range want {
		assert.Equal(t, want[i], got[i], "row %d", i)
	}
}

// TestNewReaderBytes_NextBlockRaw — the verbatim raw cursor works in-memory and
// returns slices aliasing the input buffer.
func TestNewReaderBytes_NextBlockRaw(t *testing.T) {
	t.Parallel()

	data := buildMixedLog(t)

	rm, err := reader.NewReaderBytes(data)
	require.NoError(t, err)

	var blocks int

	for {
		block, err := rm.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		require.GreaterOrEqual(t, len(block), format.FixheaderSize)
		assert.True(t, aliasesBuffer(block, data), "raw block must alias the input buffer")

		blocks++
	}

	assert.Positive(t, blocks, "at least one block")
}

// TestNewReaderBytes_Truncated — a buffer ending before the EOF marker is
// ErrTruncated, and clean io.EOF with IgnoreMissingEOF.
func TestNewReaderBytes_Truncated(t *testing.T) {
	t.Parallel()

	data := buildMixedLog(t)
	truncated := data[:len(data)-format.MarkerSize]

	rm, err := reader.NewReaderBytes(truncated)
	require.NoError(t, err)

	_, drainErr := drainAll(t, rm)
	require.ErrorIs(t, drainErr, reader.ErrTruncated, "missing EOF marker is ErrTruncated")

	rm2, err := reader.NewReaderBytes(truncated, reader.IgnoreMissingEOF())
	require.NoError(t, err)

	_, drainErr2 := drainAll(t, rm2)
	require.ErrorIs(t, drainErr2, io.EOF, "IgnoreMissingEOF downgrades to clean EOF")
}

// TestNewReaderBytes_CorruptSkip — corrupting a block's payload is caught by the
// CRC check; SkipCorruptTx resyncs to the surviving blocks.
func TestNewReaderBytes_CorruptSkip(t *testing.T) {
	t.Parallel()

	data := buildMixedLog(t)
	// Corrupt the first block's first payload byte.
	off := bytes.Index(data, format.RowMarker[:])
	require.GreaterOrEqual(t, off, 0)

	corrupt := bytes.Clone(data)
	corrupt[off+format.FixheaderSize] ^= 0xFF

	// No skip: CRC error surfaces.
	rm, err := reader.NewReaderBytes(corrupt)
	require.NoError(t, err)

	_, drainErr := drainAll(t, rm)
	require.ErrorIs(t, drainErr, reader.ErrCorruptCRC)

	// Skip: the corrupt block is dropped, the rest decode.
	rmSkip, err := reader.NewReaderBytes(corrupt, reader.SkipCorruptTx())
	require.NoError(t, err)

	rows, drainErrSkip := drainAll(t, rmSkip)
	require.ErrorIs(t, drainErrSkip, io.EOF)

	full, _ := drainAll2(t, data)
	assert.Less(t, len(rows), len(full), "at least one row dropped with the corrupt block")
}

// TestOpenMmap_EmptyFile — a 0-byte file yields a clean meta-decode error, not a
// panic or mmap failure.
func TestOpenMmap_EmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "empty.xlog")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	_, err := reader.OpenMmap(path)
	require.Error(t, err, "empty file has no meta header")
}

// --- helpers ---.

// buildMixedLog builds an in-memory xlog with several small (uncompressed) txs
// and one big (compressed, ZRow) tx, so both block paths are exercised.
func buildMixedLog(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := mustMemWriter(t, &buf)

	for lsn := int64(1); lsn <= 5; lsn++ {
		body := []byte{0xc4, 1, byte(lsn)} // bin8 of 1 byte.
		require.NoError(t, w.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: body},
		}))
	}

	// A large body to force a ZRow block.
	big := make([]byte, 0, 4096)
	big = append(big, 0xc5, 0x10, 0x00) // bin16 of 4096 bytes.
	big = append(big, make([]byte, 4096)...)
	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 6, BodyRaw: big},
	}))
	require.NoError(t, w.Close())

	return buf.Bytes()
}

// drainAll2 reads all rows from a byte image via a fresh streaming reader.
func drainAll2(t *testing.T, data []byte) ([]format.XRow, error) {
	t.Helper()

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	return drainAll(t, r)
}

// mustMemWriter builds an in-memory writer streaming into dst.
func mustMemWriter(t *testing.T, dst *bytes.Buffer) *writer.Writer {
	t.Helper()

	w, err := writer.NewWriter(dst, &format.Meta{
		Filetype: format.FiletypeXLOG,
		Version:  "go-xlog/test",
		VClock:   format.VClock{1: 0},
	})
	require.NoError(t, err)

	return w
}

// aliasesBuffer reports whether sub's backing array lies within buf — i.e. sub
// is a sub-slice of buf rather than an independent allocation. Compares the
// backing pointers, not the contents (content match would give false positives
// for short bodies that coincidentally recur).
func aliasesBuffer(sub, buf []byte) bool {
	if len(sub) == 0 || len(buf) == 0 {
		return false
	}

	subPtr := uintptr(unsafe.Pointer(&sub[0]))
	bufStart := uintptr(unsafe.Pointer(&buf[0]))
	bufEnd := bufStart + uintptr(len(buf))

	return subPtr >= bufStart && subPtr < bufEnd
}
