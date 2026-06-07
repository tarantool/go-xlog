package reader_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
)

// TestNext_SimpleXlog walks every row in simple.xlog and asserts the
// expected count (12 rows / 10 txs, with one tx of 3 rows). The
// per-fixture counts come from the fixture_test output.
func TestNext_SimpleXlog(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	n, err := drainRows(t, r)
	require.NoError(t, err, "drain")
	assert.Equal(t, 12, n, "row count")
}

func TestNext_PopulatedSnap(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "populated.snap")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	n, err := drainRows(t, r)
	require.NoError(t, err, "drain")
	assert.Equal(t, 402, n, "row count")
}

func TestNext_EmptySnap(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "empty.snap")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	n, err := drainRows(t, r)
	require.NoError(t, err, "drain")
	assert.Equal(t, 391, n, "row count")
}

// TestNextTx_GroupsMultistmt confirms logical-tx grouping: scanning
// multistmt.xlog yields at least one tx with len(Rows) >= 2, and only
// the last row of that tx has IsCommit set. Single-stmt txs come back
// as 1-row Transactions.
func TestNextTx_GroupsMultistmt(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "multistmt.xlog")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	sawMulti := false
	totalRows := 0
	totalTxs := 0

	for tx, err := range r.Txs() {
		require.NoError(t, err, "Txs yielded error")

		totalTxs++
		totalRows += len(tx.Rows)
		assert.Equal(t, tx.Rows[0].LSN, tx.StartLSN, "StartLSN = %d, Rows[0].LSN = %d", tx.StartLSN, tx.Rows[0].LSN)
		// The terminating row always has IsCommit.
		last := tx.Rows[len(tx.Rows)-1]
		assert.True(t, last.IsCommit(), "tx StartLSN=%d: last row IsCommit=false", tx.StartLSN)

		if len(tx.Rows) >= 2 {
			sawMulti = true
			// Only the last row in a multi-row tx has IsCommit.
			for i, row := range tx.Rows[:len(tx.Rows)-1] {
				assert.False(t, row.IsCommit(), "multi-row tx StartLSN=%d: non-last row %d has IsCommit", tx.StartLSN, i)
			}
		}
	}

	assert.True(t, sawMulti, "no multi-row tx seen; multistmt.xlog should contain one (3-row) tx")
	assert.Equal(t, 12, totalRows, "total rows across txs")
	assert.Equal(t, 10, totalTxs, "total tx count")
}

// TestTruncated_StrictAndLenient: chop the trailing EOF marker and
// verify ErrTruncated by default, clean io.EOF with IgnoreMissingEOF.
func TestTruncated_Strict(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	// Drop the trailing 4-byte EOF marker.
	truncated := data[:len(data)-4]
	r, err := reader.NewReader(bytes.NewReader(truncated))
	require.NoError(t, err)

	var finalErr error

	for _, err := range r.Rows() {
		if err != nil {
			finalErr = err

			break
		}
	}

	assert.ErrorIs(t, finalErr, reader.ErrTruncated, "final error")
}

func TestTruncated_Lenient(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	truncated := data[:len(data)-4]
	r, err := reader.NewReader(bytes.NewReader(truncated), reader.IgnoreMissingEOF())
	require.NoError(t, err)
	n, err := drainRows(t, r)
	require.NoError(t, err, "drain (lenient)")
	assert.Equal(t, 12, n, "row count (lenient)")
}

// TestCorruptCRC_StrictAndSkip: flip one byte inside a payload (past
// the meta) and verify ErrCorruptCRC by default, then verify
// SkipCorruptTx recovers and yields rows from the surviving txs.
func TestCorruptCRC_Strict(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	cor := corruptOneByte(t, data)
	r, err := reader.NewReader(bytes.NewReader(cor))
	require.NoError(t, err)

	var finalErr error

	for _, err := range r.Rows() {
		if err != nil {
			finalErr = err

			break
		}
	}

	assert.ErrorIs(t, finalErr, reader.ErrCorruptCRC, "final error")
}

func TestCorruptCRC_SkipRecovers(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	cor := corruptOneByte(t, data)
	r, err := reader.NewReader(bytes.NewReader(cor), reader.SkipCorruptTx())
	require.NoError(t, err)

	rows := 0

	for row, err := range r.Rows() {
		require.NoError(t, err, "Rows yielded error under SkipCorruptTx")
		require.NotNil(t, row, "Rows yielded nil row, nil error")

		rows++
	}

	assert.NotZero(t, rows, "expected to recover at least one row past the corruption, got 0")
	assert.Less(t, rows, 12, "expected fewer than 12 rows after corruption skip, got %d", rows)
}

// corruptOneByte flips a single byte at offset 200 inside the file —
// safely past the meta (the magic-byte inventory in
// testdata/README.md shows the first tx magic at offset 110, so byte
// 200 lands inside a payload). The returned slice is a copy.
func corruptOneByte(t *testing.T, data []byte) []byte {
	t.Helper()

	const targetOff = 200
	if targetOff >= len(data) {
		t.Fatalf("fixture too short to corrupt at offset %d (len=%d)", targetOff, len(data))
	}

	out := make([]byte, len(data))
	copy(out, data)
	out[targetOff] ^= 0xff

	return out
}

// TestNext_AfterEOF_StaysEOF: a Reader that has emitted io.EOF must
// keep emitting io.EOF on subsequent Next calls (no surprise reads).
func TestNext_AfterEOF_StaysEOF(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	// Drain.
	for {
		_, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err, "Next")
	}
	// Confirm idempotence.
	for i := range 3 {
		_, err := r.Next()
		assert.ErrorIs(t, err, io.EOF, "post-EOF Next %d", i)
	}
}

// TestNextTx_TruncatedMidTx: drop the last 4 bytes (EOF marker) AND
// strip the last row from the last tx so we end mid-tx. We can do this
// more simply: drop a chunk near the end large enough to truncate a tx
// payload but not so large that we drop everything. We chop the EOF
// marker plus the last small row's bytes. To stay robust without
// hand-counting offsets we rely on the looser truncation test above
// for the strict-EOF case and here verify NextTx surfaces ErrTruncated
// when bytes inside a tx payload disappear.
func TestNextTx_TruncatedMidTxPayload(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	// Drop EOF marker and the last 10 bytes of the preceding tx
	// payload — guaranteed to leave a half-read tx since the last
	// payload in the inventory is well above 10 bytes.
	truncated := data[:len(data)-14]
	r, err := reader.NewReader(bytes.NewReader(truncated))
	require.NoError(t, err)

	var lastErr error

	for {
		_, err := r.NextTx()
		if err != nil {
			lastErr = err

			break
		}
	}

	assert.ErrorIs(t, lastErr, reader.ErrTruncated, "final error")
}
