package reader_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// TestNextBlockRaw_CountAndEOF — NextBlockRaw yields one slice per physical
// block, each a well-formed framed block, then io.EOF (repeatedly).
func TestNextBlockRaw_CountAndEOF(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 5)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	var blocks int

	for {
		block, err := r.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF, "terminal error must be io.EOF")

			break
		}

		require.GreaterOrEqual(t, len(block), format.FixheaderSize, "block carries a fixheader")

		var magic [4]byte

		copy(magic[:], block[:format.MarkerSize])
		require.Contains(t, [][4]byte{format.RowMarker, format.ZRowMarker}, magic, "valid block magic")

		blocks++
	}

	assert.Equal(t, 5, blocks, "one block per single-row tx")

	// Subsequent calls keep returning io.EOF, never advancing.
	_, err = r.NextBlockRaw()
	assert.ErrorIs(t, err, io.EOF, "post-EOF call stays io.EOF")
}

// TestNextBlockRaw_EmptyLog — meta + EOF only yields io.EOF immediately.
func TestNextBlockRaw_EmptyLog(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 0)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	_, err = r.NextBlockRaw()
	assert.ErrorIs(t, err, io.EOF)
}

// TestNextBlockRaw_Truncated — a stream that ends before the EOF marker is
// ErrTruncated, and clean io.EOF with IgnoreMissingEOF.
func TestNextBlockRaw_Truncated(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 3)
	// Drop the trailing 4-byte EOF marker.
	truncated := data[:len(data)-format.MarkerSize]

	r, err := reader.NewReader(bytes.NewReader(truncated))
	require.NoError(t, err)

	// Three good blocks, then a truncation.
	for range 3 {
		_, berr := r.NextBlockRaw()
		require.NoError(t, berr)
	}

	_, err = r.NextBlockRaw()
	require.ErrorIs(t, err, reader.ErrTruncated, "missing EOF marker is ErrTruncated")

	// IgnoreMissingEOF downgrades to clean io.EOF.
	r2, err := reader.NewReader(bytes.NewReader(truncated), reader.IgnoreMissingEOF())
	require.NoError(t, err)

	for range 3 {
		_, berr := r2.NextBlockRaw()
		require.NoError(t, berr)
	}

	_, err = r2.NextBlockRaw()
	require.ErrorIs(t, err, io.EOF, "IgnoreMissingEOF yields clean io.EOF")
}

// TestNextBlockRaw_SliceReused — the returned slice aliases reusable scratch and
// is clobbered on the next call (documents the contract).
func TestNextBlockRaw_SliceReused(t *testing.T) {
	t.Parallel()

	data := buildLog(t, 2)

	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	b1, err := r.NextBlockRaw()
	require.NoError(t, err)

	// Copy out before advancing.
	saved := bytes.Clone(b1)

	b2, err := r.NextBlockRaw()
	require.NoError(t, err)

	// The two physical blocks differ (distinct bodies), proving b2 is fresh
	// content and the caller must copy to retain b1.
	assert.NotEqual(t, saved, b2, "consecutive blocks differ")
}
