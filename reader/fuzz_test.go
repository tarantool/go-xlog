package reader_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// requireRowEqual asserts want == got, treating two NaN timestamps as equal
// (a NaN timestamp decodes deterministically but reflect.DeepEqual considers
// NaN != NaN, which would be a false mismatch in a differential test).
func requireRowEqual(t *testing.T, want, got format.XRow, msgAndArgs ...any) {
	t.Helper()

	if math.IsNaN(want.Timestamp) && math.IsNaN(got.Timestamp) {
		want.Timestamp, got.Timestamp = 0, 0
	}

	require.Equal(t, want, got, msgAndArgs...)
}

// FuzzReader feeds arbitrary bytes to the streaming reader and drains it,
// asserting the crash-resistance property: NewReader + Rows never panic,
// hang, or OOM on malformed input — they return an error or stop cleanly.
func FuzzReader(f *testing.F) {
	// Seed from every committed fixture: the flat testdata/ files and the
	// historical corpus (all five artefact types).
	for _, root := range []string{filepath.Join("..", "testdata")} {
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil //nolint:nilerr // fuzz: malformed inputs are skipped, not failures
			}

			switch filepath.Ext(p) {
			case ".xlog", ".snap", ".vylog", ".run", ".index":
				if b, err := os.ReadFile(p); err == nil {
					f.Add(b)
				}
			}

			return nil
		})
	}

	f.Add([]byte("XLOG\n0.13\n\n"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := reader.NewReader(bytes.NewReader(b), reader.AcceptV012())
		if err != nil {
			return // Malformed meta — rejected cleanly, the expected path.
		}
		// Drain, bounded so a pathological input can't loop forever.
		const capacity = 1 << 20

		n := 0

		for _, err := range r.Rows() {
			if err != nil {
				break
			}

			if n++; n > capacity {
				require.LessOrEqual(t, n, capacity, "row stream exceeded %d rows — possible unbounded loop", capacity)
			}
		}
	})
}

// addLogSeeds seeds f with every committed log fixture under testdata/, plus a
// couple of minimal hand-built inputs. Shared by the differential fuzzers.
func addLogSeeds(f *testing.F) {
	f.Helper()

	_ = filepath.Walk(filepath.Join("..", "testdata"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil //nolint:nilerr // fuzz: unreadable seeds are skipped, not failures
		}

		switch filepath.Ext(p) {
		case ".xlog", ".snap", ".vylog", ".run", ".index":
			if b, err := os.ReadFile(p); err == nil {
				f.Add(b)
			}
		}

		return nil
	})

	f.Add([]byte("XLOG\n0.13\n\n"))
	f.Add([]byte{})
}

// drainRows reads every row from b via the proven Next path, returning the
// rows collected and whether the stream terminated at a clean io.EOF (as
// opposed to a mid-stream error). NewReader is required to succeed.
func drainRowsFromBytes(t *testing.T, b []byte) ([]format.XRow, bool) {
	t.Helper()

	r, err := reader.NewReader(bytes.NewReader(b), reader.AcceptV012())
	require.NoError(t, err, "drainRows: NewReader")

	defer func() { _ = r.Close() }()

	var rows []format.XRow

	const capacity = 1 << 20

	for n := 0; ; n++ {
		row, err := r.Next()
		if err != nil {
			return rows, errors.Is(err, io.EOF)
		}

		rows = append(rows, row)

		require.LessOrEqual(t, n, capacity, "drainRows: stream exceeded %d rows", capacity)
	}
}

// FuzzNextBlockRaw pins the verbatim block-copy path (NextBlockRaw +
// writer.WriteRawBlock) to the proven Next decoder. For any input whose meta
// parses, draining NextBlockRaw must never panic, hang, or OOM. Whenever
// the raw drain completes cleanly (reaches the EOF marker, every block
// CRC-valid), re-emitting those blocks verbatim into a fresh log and decoding it
// with Next must reproduce exactly the rows — and the same terminal state — that
// Next yields over the original bytes. This proves the forwarded bytes are a
// faithful copy down to each row.
func FuzzNextBlockRaw(f *testing.F) {
	addLogSeeds(f)

	f.Fuzz(func(t *testing.T, b []byte) {
		rr, errR := reader.NewReader(bytes.NewReader(b), reader.AcceptV012())
		if errR != nil {
			return // Malformed meta — rejected cleanly.
		}

		defer func() { _ = rr.Close() }()

		var dst bytes.Buffer

		w, err := writer.NewWriter(&dst, &format.Meta{
			Filetype: format.FiletypeXLOG,
			Version:  "go-xlog/fuzz",
			VClock:   format.VClock{1: 0},
		})
		require.NoError(t, err, "dst writer")

		const capacity = 1 << 20

		rawClean := true

		for n := 0; ; n++ {
			block, err := rr.NextBlockRaw()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					rawClean = false
				}

				break
			}

			require.NoError(t, w.WriteRawBlock(block), "WriteRawBlock of a CRC-validated block")
			require.LessOrEqual(t, n, capacity, "block stream exceeded %d blocks", capacity)
		}

		if !rawClean {
			return // Corrupt/truncated source: no faithful round-trip to assert.
		}

		require.NoError(t, w.Close(), "dst Close")

		// Differential: decoding the rebuilt log must match decoding the
		// original, row-for-row and in terminal state (both rely on Next, so a
		// valid-CRC-but-undecodable payload errors identically on both sides).
		want, wantEOF := drainRowsFromBytes(t, b)
		got, gotEOF := drainRowsFromBytes(t, dst.Bytes())

		require.Equal(t, wantEOF, gotEOF, "terminal state differs after verbatim copy")
		require.Len(t, got, len(want), "row count differs after verbatim copy")

		for i := range want {
			requireRowEqual(t, want[i], got[i], "row %d differs after verbatim copy", i)
		}
	})
}

// FuzzReaderBytesMatchesStream pins the in-memory cursor (NewReaderBytes, used
// by OpenMmap) to the proven streaming reader: over the same bytes, the two must
// accept/reject the meta together and then yield exactly the same rows — header
// fields and BodyRaw — and reach the same terminal state. Bodies are cloned
// before compare because the streaming reader aliases its reused read buffer.
func FuzzReaderBytesMatchesStream(f *testing.F) {
	addLogSeeds(f)

	f.Fuzz(func(t *testing.T, b []byte) {
		rs, errS := reader.NewReader(bytes.NewReader(b), reader.AcceptV012())
		rm, errM := reader.NewReaderBytes(b, reader.AcceptV012())

		// Meta parsing is deterministic: both accept or both reject.
		require.Equal(t, errS != nil, errM != nil, "meta accept disagreement (bytes vs stream)")

		if errS != nil {
			return
		}

		defer func() { _ = rs.Close() }()
		defer func() { _ = rm.Close() }()

		const capacity = 1 << 20

		for n := 0; ; n++ {
			srow, serr := rs.Next()
			mrow, merr := rm.Next()

			if serr != nil {
				require.Error(t, merr, "stream errored at row %d but bytes did not", n)
				require.Equal(t, errors.Is(serr, io.EOF), errors.Is(merr, io.EOF),
					"terminal EOF disagreement: stream=%v bytes=%v", serr, merr)

				return
			}

			require.NoError(t, merr, "bytes errored at row %d but stream did not", n)

			// Clone the stream body (it aliases the reused read buffer) before
			// the whole-struct compare; the bytes body aliases the stable input.
			srow.BodyRaw = bytes.Clone(srow.BodyRaw)
			requireRowEqual(t, srow, mrow, "row %d differs (bytes vs stream)", n)

			require.LessOrEqual(t, n, capacity, "row stream exceeded %d rows", capacity)
		}
	})
}

// FuzzScanMatchesNext is a differential fuzzer pinning the zero-alloc arena
// cursor (Scan/Row, in both the default body-copy and WithAliasBodies modes) to
// the proven Next path: over the same bytes, every Scan must yield exactly the
// row Next yields — all header fields and BodyRaw — and the three readers must
// reach end-of-stream (or the same error class) in lockstep. This covers the
// new arena/body-copy code that FuzzReader (which only drives Rows) never
// touches.
func FuzzScanMatchesNext(f *testing.F) {
	addLogSeeds(f)

	f.Fuzz(func(t *testing.T, b []byte) {
		rn, errN := reader.NewReader(bytes.NewReader(b), reader.AcceptV012())
		rc, errC := reader.NewReader(bytes.NewReader(b), reader.AcceptV012())
		ra, errA := reader.NewReader(bytes.NewReader(b), reader.AcceptV012(), reader.WithAliasBodies())

		// Meta parsing is deterministic: all three accept or all reject.
		require.Equal(t, errN != nil, errC != nil, "meta accept disagreement (copy)")
		require.Equal(t, errN != nil, errA != nil, "meta accept disagreement (alias)")

		if errN != nil {
			return
		}

		defer func() { _ = rn.Close() }()
		defer func() { _ = rc.Close() }()
		defer func() { _ = ra.Close() }()

		const capacity = 1 << 20

		for n := 0; ; n++ {
			nrow, nerr := rn.Next()
			okC := rc.Scan()
			okA := ra.Scan()

			if nerr != nil {
				require.False(t, okC, "Scan(copy) yielded a row where Next errored: %v", nerr)
				require.False(t, okA, "Scan(alias) yielded a row where Next errored: %v", nerr)

				if errors.Is(nerr, io.EOF) {
					require.NoError(t, rc.Err(), "Scan(copy) Err at clean EOF")
					require.NoError(t, ra.Err(), "Scan(alias) Err at clean EOF")
				} else {
					require.Error(t, rc.Err(), "Scan(copy) must surface the error Next saw")
					require.Error(t, ra.Err(), "Scan(alias) must surface the error Next saw")
				}

				return
			}

			require.True(t, okC, "Scan(copy) stopped early; Err: %v", rc.Err())
			require.True(t, okA, "Scan(alias) stopped early; Err: %v", ra.Err())

			// Whole-struct compare (all header fields + BodyRaw bytes), done
			// while the alias body is still valid (before the next Scan).
			requireRowEqual(t, nrow, rc.Row(), "Scan(copy) row %d differs from Next", n)
			requireRowEqual(t, nrow, ra.Row(), "Scan(alias) row %d differs from Next", n)

			require.LessOrEqual(t, n, capacity, "row stream exceeded %d rows", capacity)
		}
	})
}
