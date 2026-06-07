package writer_test

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// srcXlog is the large bulk-load fixture sitting in the repo's scratch
// tmp-test/ dir (1,000,002 single-row autocommit INSERTs into space 512,
// zstd-compressed ~50 rows per block). It is NOT a committed testdata
// fixture, so the test skips when it is absent (e.g. in CI).
const srcXlog = "../tmp-test/00000000000000000000.xlog"

// blockFlushBytes is the accumulated BodyRaw budget after which we flush a
// tx block. Chosen well above format.CompressThreshold (2 KiB) so every
// flushed block crosses the threshold and is zstd-compressed (ZRowMarker).
const blockFlushBytes = 16 * 1024

// firstUserSpaceID is the lowest user-space id; everything below it is a
// system space (Tarantool BOX_SYSTEM_ID_MAX = 511). Rows targeting a system
// space carry schema/DDL (e.g. the _space and _index inserts that create the
// data space and its primary index) and must be preserved for the output to
// stay recoverable.
const firstUserSpaceID = 512

// TestDumpEvenDataRecords reads every row from srcXlog and writes a new xlog
// that drops every other *data-insertion* record (user spaces, id >= 512)
// while keeping ALL schema/DDL rows, packed into zstd-compressed blocks.
//
// Filtering only data rows is what keeps the result loadable: a blind
// every-other-row filter would drop the _index DDL (it sits at an odd stream
// position), leaving INSERTs against an index-less space — Tarantool recovery
// then fails with "No index #0 is defined". Preserving system-space rows
// keeps the CREATE space + CREATE index DDL intact, so the half-size log
// recovers cleanly (verified out-of-band against a real Tarantool).
//
// It uses a BatchWriter so each kept record stays its own single-row
// autocommit transaction (matching the all-autocommit source) while still
// sharing compressed blocks — the property plain WriteTx/CommitTx batching
// cannot provide, since those merge a block's rows into one logical tx.
//
// Verified end-to-end:
//   - schema preserved: every system-space row survives;
//   - selection fidelity: the LSN sequence read back equals the LSNs of the
//     kept source rows (assignTxIDs rewrites TSN but never LSN, so LSNs
//     uniquely identify each retained record across the round trip);
//   - structure: every output transaction is a single row (autocommit
//     preserved) and the blocks are zstd-compressed.
func TestDumpEvenDataRecords(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping large-fixture dump in -short mode")
	}

	if _, err := os.Stat(srcXlog); errors.Is(err, os.ErrNotExist) {
		t.Skipf("source xlog %s not present (scratch fixture, not committed)", srcXlog)
	}

	src, err := reader.Open(srcXlog)
	require.NoError(t, err)

	defer func() { _ = src.Close() }()

	outPath := filepath.Join(t.TempDir(), "even.xlog")

	// Clone the source meta so the output carries the same filetype,
	// instance UUID, and format version.
	w, err := writer.Create(outPath, src.Meta().Clone())
	require.NoError(t, err)

	// Pack independent autocommit txs into ~blockFlushBytes compressed blocks.
	bw := writer.NewBatchWriter(w, writer.BatchOptions{MaxBytes: blockFlushBytes})

	var (
		rows        int     // Total source rows seen.
		dataIdx     int     // 0-based index among data-insertion records only.
		schemaTotal int     // System-space rows seen.
		schemaKept  int     // System-space rows kept (must equal schemaTotal).
		wantLSNs    []int64 // LSNs of the records we keep, in order.
	)

	for row, err := range src.Rows() {
		require.NoError(t, err)

		rows++

		// A data-insertion record targets a user space (id >= 512); anything
		// else (system spaces, or non-DML rows we can't decode) is schema.
		isData := false
		if b, derr := format.DecodeDMLBody(row.BodyRaw); derr == nil && b.SpaceID >= firstUserSpaceID {
			isData = true
		}

		keep := true

		if isData {
			keep = dataIdx%2 == 0 // Drop every other data record.
			dataIdx++
		} else {
			schemaTotal++

			if keep {
				schemaKept++
			}
		}

		if keep {
			// The reader reuses its decode buffers, so a row's BodyRaw is only
			// valid until the next Next(). The BatchWriter retains rows until
			// their block flushes, so deep-copy before handing it over.
			require.NoError(t, bw.WriteTx([]format.XRow{cloneRow(row)}))

			wantLSNs = append(wantLSNs, row.LSN)
		}
	}

	// Close flushes the trailing block, writes the EOF marker, and renames
	// the .inprogress file into place.
	require.NoError(t, bw.Close())

	t.Logf("source rows=%d, kept=%d (%d schema + %d/%d data)",
		rows, len(wantLSNs), schemaKept, dataIdx/2, dataIdx)
	require.NotEmpty(t, wantLSNs, "expected at least one kept record")
	assert.Equal(t, schemaTotal, schemaKept, "all schema/DDL rows must be preserved")

	srcInfo, err := os.Stat(srcXlog)
	require.NoError(t, err)

	outInfo, err := os.Stat(outPath)
	require.NoError(t, err)

	t.Logf("source xlog=%d bytes, even-only xlog=%d bytes (%.1f%%)",
		srcInfo.Size(), outInfo.Size(),
		100*float64(outInfo.Size())/float64(srcInfo.Size()))

	// --- read the output back and verify record fidelity ---.
	out, err := reader.Open(outPath)
	require.NoError(t, err)

	defer func() { _ = out.Close() }()

	assert.Equal(t, src.Meta().Filetype, out.Meta().Filetype, "filetype preserved")

	gotLSNs := make([]int64, 0, len(wantLSNs))
	multiRowTxs := 0

	for tx, err := range out.Txs() {
		require.NoError(t, err)

		if len(tx.Rows) != 1 {
			multiRowTxs++
		}

		for _, row := range tx.Rows {
			gotLSNs = append(gotLSNs, row.LSN)
		}
	}

	require.Len(t, gotLSNs, len(wantLSNs), "output row count")
	assert.Equal(t, wantLSNs, gotLSNs, "output holds exactly the kept records (all schema + even data), in order")
	assert.Zero(t, multiRowTxs, "every record stays its own autocommit tx")

	// --- verify the output is actually zstd-compressed ---.
	zrow, plain := countBlockMagics(t, outPath)
	t.Logf("output blocks: zstd(ZRowMarker)=%d plain(RowMarker)=%d", zrow, plain)
	assert.Positive(t, zrow, "expected zstd-compressed blocks in the output")
}

// cloneRow returns a deep copy of row whose BodyRaw is an independent
// allocation, safe to retain past the reader's next Next() call.
func cloneRow(row format.XRow) format.XRow {
	row.BodyRaw = append([]byte(nil), row.BodyRaw...)

	return row
}

// countBlockMagics walks the on-disk tx-block fixheaders of an xlog and
// returns the number of zstd (ZRowMarker) and plain (RowMarker) blocks.
// It parses the framing directly because neither reader nor format exposes
// per-block compression state through their public row/tx APIs.
func countBlockMagics(t *testing.T, path string) (int, int) {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)

	defer func() { _ = f.Close() }()

	br := bufio.NewReader(f)

	_, err = format.DecodeMeta(br, format.MetaOptions{})
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
