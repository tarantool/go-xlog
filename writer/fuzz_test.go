package writer //nolint:testpackage // shares internal test helpers (newMeta) with white-box tests in writer_test.go

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// FuzzWriterRoundTrip asserts read∘write = identity and crash-resistance:
// a row set derived from the fuzz bytes is written via one of the three
// write paths (with/without compression), then read back and compared. Bodies
// are valid msgpack so the reader can delimit them; the writer copies them
// verbatim, so they must round-trip byte-exact.
//
// Runs fully in memory (NewWriter → bytes.Buffer → reader.NewReader): no
// filesystem, no fsync, no rename — for a high fuzz exec rate.
func FuzzWriterRoundTrip(f *testing.F) {
	f.Add([]byte("alpha"))
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x01, 0x02, 0x03})
	f.Add(bytes.Repeat([]byte("z"), 5000)) // Crosses the 2 KiB zstd threshold.

	f.Fuzz(func(t *testing.T, b []byte) {
		// First two bytes pick compression + write path; the rest is payload.
		noComp := len(b) > 0 && b[0]&1 == 1

		path := 0
		if len(b) > 1 {
			path = int(b[1]) % 3
		}

		payload := b
		if len(b) > 2 {
			payload = b[2:]
		}

		rows := deriveRows(payload)

		var (
			buf  bytes.Buffer
			opts []Option
		)

		if noComp {
			opts = append(opts, NoCompression())
		}

		w, err := NewWriter(&buf, newMeta(t), opts...)
		require.NoError(t, err, "NewWriter")

		switch path {
		case 0: // Single multi-row transaction.
			if len(rows) > 0 {
				require.NoError(t, w.WriteTx(rows), "WriteTx")
			}
		case 1: // One single-statement tx per row.
			for _, r := range rows {
				require.NoError(t, w.WriteTx([]format.XRow{r}), "WriteTx single")
			}
		case 2: // Append-then-commit.
			for _, r := range rows {
				require.NoError(t, w.WriteRow(r), "WriteRow")
			}

			if len(rows) > 0 {
				require.NoError(t, w.CommitTx(), "CommitTx")
			}
		}

		require.NoError(t, w.Close(), "Close")

		// Read back and compare the flat (Type, LSN, body) sequence.
		rd, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
		require.NoError(t, err, "reader.NewReader")

		defer func() { _ = rd.Close() }()

		var got []format.XRow

		for row, err := range rd.Rows() {
			require.NoError(t, err, "Rows")

			cp := row
			cp.BodyRaw = append([]byte(nil), row.BodyRaw...)
			got = append(got, cp)
		}

		require.Len(t, got, len(rows), "row count")

		for i := range rows {
			assert.Equal(t, rows[i].Type, got[i].Type, "row[%d] Type", i)
			assert.Equal(t, rows[i].LSN, got[i].LSN, "row[%d] LSN", i)
			assert.Equal(t, rows[i].BodyRaw, got[i].BodyRaw, "row[%d] body", i)
		}
	})
}

// deriveRows carves payload into up to 64 rows, each carrying a valid msgpack
// body (a bin of the chunk) and a sequential LSN starting at 1.
func deriveRows(payload []byte) []format.XRow {
	if len(payload) == 0 {
		return nil
	}

	const maxRows = 64

	chunks := min(1+len(payload)/16, maxRows)
	size := (len(payload) + chunks - 1) / chunks

	var rows []format.XRow

	lsn := int64(1)

	for i := 0; i < len(payload); i += size {
		end := min(i+size, len(payload))

		body, err := msgpack.Marshal(payload[i:end])
		if err != nil {
			return rows
		}

		rows = append(rows, format.XRow{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: body})
		lsn++
	}

	return rows
}

// FuzzBatchRoundTrip drives the eager-encode BatchWriter (the new path that
// encodes each tx into the pending byte buffer immediately and compresses the
// whole buffer at flush) with an arbitrary row set and flush policy, then reads
// the result back and asserts every row round-trips (Type/LSN/body) and reads
// back as its own committed autocommit tx. FuzzWriterRoundTrip never exercises
// BatchWriter, so this closes that gap.
func FuzzBatchRoundTrip(f *testing.F) {
	f.Add([]byte("alpha"))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte("z"), 5000)) // Crosses the zstd threshold once batched.

	f.Fuzz(func(t *testing.T, b []byte) {
		// First byte selects the flush policy; 0 disables MaxTxs (flush at Close).
		maxTxs := 0
		if len(b) > 0 {
			maxTxs = int(b[0]) % 8
		}

		payload := b
		if len(b) > 1 {
			payload = b[1:]
		}

		rows := deriveRows(payload)
		if len(rows) == 0 {
			return
		}

		var buf bytes.Buffer

		w, err := NewWriter(&buf, newMeta(t))
		require.NoError(t, err, "NewWriter")

		bw := NewBatchWriter(w, BatchOptions{MaxTxs: maxTxs})
		for _, r := range rows {
			require.NoError(t, bw.WriteTx([]format.XRow{r}), "BatchWriter.WriteTx")
		}

		require.NoError(t, bw.Close(), "BatchWriter.Close")

		rd, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
		require.NoError(t, err, "reader.NewReader")

		defer func() { _ = rd.Close() }()

		var got []format.XRow

		for row, err := range rd.Rows() {
			require.NoError(t, err, "Rows")

			cp := row
			cp.BodyRaw = append([]byte(nil), row.BodyRaw...)
			got = append(got, cp)
		}

		require.Len(t, got, len(rows), "row count round-tripped")

		for i := range rows {
			assert.Equal(t, rows[i].Type, got[i].Type, "row[%d] Type", i)
			assert.Equal(t, rows[i].LSN, got[i].LSN, "row[%d] LSN", i)
			assert.Equal(t, rows[i].BodyRaw, got[i].BodyRaw, "row[%d] body", i)
			assert.True(t, got[i].IsCommit(), "row[%d] is a single-row autocommit tx", i)
		}
	})
}
