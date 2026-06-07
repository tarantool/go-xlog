package writer //nolint:testpackage // shares internal test helpers (newMeta, encodeDMLBody, fixedDMLBody) with white-box tests

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// TestBatchWriter_NoCloneFromAliasingReader is the read→batch-write pipeline
// (the caching dumper): rows are fed straight from a reader configured with
// WithAliasBodies — so each Row().BodyRaw aliases the reader's read buffer and
// is clobbered on the next Scan — directly into a BatchWriter with NO cloning.
// The eager-encode BatchWriter consumes each row on the call, so the output
// must still round-trip byte-for-byte. Under the old retain-until-flush
// BatchWriter this would corrupt every buffered body.
func TestBatchWriter_NoCloneFromAliasingReader(t *testing.T) {
	t.Parallel()

	const ntx = 120 // > one flush window, forces real block boundaries.

	// Build a source log.
	var src bytes.Buffer

	sw, err := NewWriter(&src, newMeta(t))
	require.NoError(t, err)

	wantBodies := make([][]byte, ntx)

	for i := range ntx {
		body := encodeDMLBody([]uint64{uint64(i), uint64(i * 7)})
		wantBodies[i] = body
		require.NoError(t, sw.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: int64(i + 1), BodyRaw: body}}))
	}

	require.NoError(t, sw.Close())

	// Read with aliasing bodies, feed straight into a BatchWriter — no clone.
	rd, err := reader.NewReader(bytes.NewReader(src.Bytes()), reader.WithAliasBodies())
	require.NoError(t, err)

	defer func() { _ = rd.Close() }()

	var dst bytes.Buffer

	dw, err := NewWriter(&dst, newMeta(t))
	require.NoError(t, err)

	bw := NewBatchWriter(dw, BatchOptions{MaxTxs: 50})

	for rd.Scan() {
		row := rd.Row() // BodyRaw aliases the read buffer; valid only until next Scan.
		require.NoError(t, bw.WriteTx([]format.XRow{row}))
	}

	require.NoError(t, rd.Err())
	require.NoError(t, bw.Close())

	// Re-read the output; bodies must match the source exactly.
	out, err := reader.NewReader(bytes.NewReader(dst.Bytes()))
	require.NoError(t, err)

	defer func() { _ = out.Close() }()

	got := 0

	for out.Scan() {
		require.Equal(t, wantBodies[got], out.Row().BodyRaw, "body[%d]", got)

		got++
	}

	require.NoError(t, out.Err())
	assert.Equal(t, ntx, got, "row count round-tripped")
}

// TestWriteBlock_PreservesLogicalTxs stamps three transactions of sizes
// 1, 2, 1 (via assignTxIDs, as a caller or BatchWriter would), writes them as
// one verbatim block, and asserts the reader reconstructs all three as
// distinct logical transactions from the row flags — while only one physical
// tx block lands on disk.
func TestWriteBlock_PreservesLogicalTxs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w, err := NewWriter(&buf, newMeta(t))
	require.NoError(t, err)

	tx1 := []format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1})}}
	tx2 := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: encodeDMLBody([]uint64{2})},
		{Type: iproto.IPROTO_INSERT, LSN: 3, BodyRaw: encodeDMLBody([]uint64{3})},
	}
	tx3 := []format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 4, BodyRaw: encodeDMLBody([]uint64{4})}}

	// Stamp each tx's TSN/commit flags, then concatenate into one flat block.
	assignTxIDs(tx1)
	assignTxIDs(tx2)
	assignTxIDs(tx3)

	block := make([]format.XRow, 0, 4)
	block = append(block, tx1...)
	block = append(block, tx2...)
	block = append(block, tx3...)

	require.NoError(t, w.WriteBlock(block))
	require.NoError(t, w.Close())

	assert.Equal(t, []int{1, 2, 1}, readTxSizes(t, buf.Bytes()), "logical tx shapes preserved")

	zrow, plain := countTxBlocks(t, buf.Bytes())
	assert.Equal(t, 1, zrow+plain, "all txs land in exactly one physical block")
}

// TestWriteBlock_Validation covers the error paths.
func TestWriteBlock_Validation(t *testing.T) {
	t.Parallel()

	row := func(lsn uint64) format.XRow {
		return format.XRow{Type: iproto.IPROTO_INSERT, LSN: int64(lsn), BodyRaw: encodeDMLBody([]uint64{lsn})}
	}

	t.Run("empty rows", func(t *testing.T) {
		t.Parallel()

		w := newMemWriter(t)
		require.ErrorIs(t, w.WriteBlock(nil), ErrEmptyBlock)
		require.ErrorIs(t, w.WriteBlock([]format.XRow{}), ErrEmptyBlock)
	})

	t.Run("pending WriteRow buffer", func(t *testing.T) {
		t.Parallel()

		w := newMemWriter(t)
		require.NoError(t, w.WriteRow(row(1)))
		require.ErrorIs(t, w.WriteBlock([]format.XRow{row(2)}), ErrPendingWriteRow)
	})

	t.Run("after close", func(t *testing.T) {
		t.Parallel()

		w := newMemWriter(t)
		require.NoError(t, w.Close())
		require.ErrorIs(t, w.WriteBlock([]format.XRow{row(1)}), ErrClosed)
	})
}

// TestBatchWriter_AutocommitStaysAutocommit feeds many single-row autocommit
// transactions through a BatchWriter and asserts the two properties that
// plain WriteTx cannot deliver together: every record reads back as its own
// autocommit tx (logical identity preserved) AND the records are packed into
// a few zstd-compressed blocks (physical batching).
func TestBatchWriter_AutocommitStaysAutocommit(t *testing.T) {
	t.Parallel()

	const (
		numTxs   = 20
		maxTxs   = 5
		bodyPad  = 512 // Bytes per row body so each 5-tx block crosses CompressThreshold.
		wantZrow = numTxs / maxTxs
	)

	var buf bytes.Buffer

	w, err := NewWriter(&buf, newMeta(t))
	require.NoError(t, err)

	bw := NewBatchWriter(w, BatchOptions{MaxTxs: maxTxs})

	for i := range numTxs {
		row := format.XRow{Type: iproto.IPROTO_INSERT, LSN: int64(i + 1), BodyRaw: fixedDMLBody(512, bodyPad)}
		require.NoError(t, bw.WriteTx([]format.XRow{row}))
	}

	require.NoError(t, bw.Close())

	sizes := readTxSizes(t, buf.Bytes())
	require.Len(t, sizes, numTxs, "every record is its own logical tx")

	for i, n := range sizes {
		assert.Equalf(t, 1, n, "tx %d should be a single-row autocommit tx", i)
	}

	zrow, plain := countTxBlocks(t, buf.Bytes())
	assert.Equal(t, wantZrow, zrow, "records packed into compressed blocks")
	assert.Zero(t, plain, "every block should be over the compression threshold")
}

// TestBatchWriter_FlushBoundaries checks MaxTxs and MaxBytes each trigger a
// block flush, and that Close emits the trailing partial block.
func TestBatchWriter_FlushBoundaries(t *testing.T) {
	t.Parallel()

	row := func(lsn int64) format.XRow {
		return format.XRow{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: fixedDMLBody(600, 512)}
	}

	t.Run("MaxTxs", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		w, err := NewWriter(&buf, newMeta(t))
		require.NoError(t, err)

		// 7 txs at MaxTxs=3 → blocks of 3, 3, 1.
		bw := NewBatchWriter(w, BatchOptions{MaxTxs: 3})
		for i := range 7 {
			require.NoError(t, bw.WriteTx([]format.XRow{row(int64(i + 1))}))
		}

		require.NoError(t, bw.Close())

		zrow, plain := countTxBlocks(t, buf.Bytes())
		assert.Equal(t, 3, zrow+plain, "7 txs / 3 per block → 3 blocks")
		assert.Len(t, readTxSizes(t, buf.Bytes()), 7)
	})

	t.Run("MaxBytes", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		w, err := NewWriter(&buf, newMeta(t))
		require.NoError(t, err)

		// Each body is ~520 bytes; MaxBytes=1000 flushes after every 2nd tx.
		bw := NewBatchWriter(w, BatchOptions{MaxBytes: 1000})
		for i := range 6 {
			require.NoError(t, bw.WriteTx([]format.XRow{row(int64(i + 1))}))
		}

		require.NoError(t, bw.Close())

		zrow, plain := countTxBlocks(t, buf.Bytes())
		assert.Equal(t, 3, zrow+plain, "6 txs, flush every 2 → 3 blocks")
		assert.Len(t, readTxSizes(t, buf.Bytes()), 6)
	})
}

// TestBatchWriter_FlushEmptyAndReuse confirms Flush is a no-op when nothing is
// buffered and that the writer keeps working across flushes.
func TestBatchWriter_FlushEmptyAndReuse(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w, err := NewWriter(&buf, newMeta(t))
	require.NoError(t, err)

	bw := NewBatchWriter(w, BatchOptions{MaxTxs: 2})
	require.NoError(t, bw.Flush()) // Nothing buffered.

	require.NoError(t, bw.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1})}}))
	require.NoError(t, bw.Flush()) // Explicit flush of one tx.
	require.NoError(t, bw.Flush()) // again: no-op
	require.NoError(t, bw.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: encodeDMLBody([]uint64{2})}}))
	require.NoError(t, bw.Close())

	assert.Equal(t, []int{1, 1}, readTxSizes(t, buf.Bytes()))

	zrow, plain := countTxBlocks(t, buf.Bytes())
	assert.Equal(t, 2, zrow+plain, "two explicit/closing flushes → two blocks")
}

// newMemWriter returns an in-memory Writer for validation tests where the
// output bytes are irrelevant.
func newMemWriter(t *testing.T) *Writer {
	t.Helper()

	w, err := NewWriter(&bytes.Buffer{}, newMeta(t))
	require.NoError(t, err)

	return w
}

// readTxSizes reads raw back through the reader and returns the row count of
// each logical transaction, in order.
func readTxSizes(t *testing.T, raw []byte) []int {
	t.Helper()

	r, err := reader.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	var sizes []int

	for tx, err := range r.Txs() {
		require.NoError(t, err)

		sizes = append(sizes, len(tx.Rows))
	}

	return sizes
}

// countTxBlocks walks the on-disk fixheaders of raw and counts zstd
// (ZRowMarker) and plain (RowMarker) tx blocks.
func countTxBlocks(t *testing.T, raw []byte) (int, int) {
	t.Helper()

	br := bufio.NewReader(bytes.NewReader(raw))

	_, err := format.DecodeMeta(br, format.MetaOptions{})
	require.NoError(t, err)

	var (
		fh          [format.FixheaderSize]byte
		zrow, plain int
	)

	for {
		peek, err := br.Peek(format.MarkerSize)
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		var magic [4]byte

		copy(magic[:], peek)

		if magic == format.EOFMarker {
			break
		}

		_, err = io.ReadFull(br, fh[:])
		require.NoError(t, err)

		h, err := format.DecodeFixheader(fh)
		require.NoError(t, err)

		switch h.Magic {
		case format.ZRowMarker:
			zrow++
		case format.RowMarker:
			plain++
		}

		_, err = br.Discard(int(h.Len))
		require.NoError(t, err)
	}

	return zrow, plain
}
