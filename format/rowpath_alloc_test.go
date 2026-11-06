package format_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// The tests of this file lock in the zero-allocation property of the row path
// a replication consumer runs for every row: the header (DecodeXRowInto), the
// body (DecodeDMLBodyInto) and the operations of an UPDATE or UPSERT
// (DecodeOpsInto). Each path is also checked on its own next to its decoder;
// here the whole chain is. They assert == 0 exactly, not a threshold: one
// allocation per row is the regression that matters. They are not parallel,
// as AllocsPerRun counts the mallocs of the whole process.

// replicationRow is a row as a relay delivers it: every header key of a wire
// row, and the body of an UPDATE on a secondary index issued from Lua.
func replicationRow(t *testing.T) []byte {
	t.Helper()

	row := benchFullRow()
	row.Type = iproto.IPROTO_UPDATE
	row.BodyRaw = dmlIndexBody()

	enc, err := format.EncodeXRow(nil, row)
	require.NoError(t, err)

	return enc
}

// rowPath decodes one row the way a replication consumer does, reusing every
// destination, and reports how many operations the row carried.
type rowPath struct {
	row  format.XRow
	body format.DMLBody
	ops  []format.Operation
}

func (p *rowPath) decode(b []byte) (int, error) {
	if _, err := format.DecodeXRowInto(b, &p.row); err != nil {
		return 0, fmt.Errorf("header: %w", err)
	}

	switch p.row.Type { //nolint:exhaustive // only the DML rows have a DML body
	case iproto.IPROTO_INSERT, iproto.IPROTO_REPLACE, iproto.IPROTO_DELETE,
		iproto.IPROTO_UPDATE, iproto.IPROTO_UPSERT:
	default:
		return 0, nil
	}

	if err := format.DecodeDMLBodyInto(p.row.BodyRaw, &p.body, nil); err != nil {
		return 0, fmt.Errorf("body: %w", err)
	}

	// The operations of an UPDATE are its IPROTO_TUPLE, those of an UPSERT
	// its IPROTO_OPS.
	var ops []byte

	switch p.row.Type { //nolint:exhaustive // only UPDATE and UPSERT carry operations
	case iproto.IPROTO_UPDATE:
		ops = p.body.Tuple
	case iproto.IPROTO_UPSERT:
		ops = p.body.Ops
	default:
		return 0, nil
	}

	var err error

	p.ops, err = format.DecodeOpsInto(ops, p.ops)

	return len(p.ops), err
}

func TestRowPath_ZeroAlloc(t *testing.T) { //nolint:paralleltest // AllocsPerRun counts process-wide mallocs
	if testing.Short() {
		t.Skip("allocation counts: skipped in -short mode")
	}

	enc := replicationRow(t)

	p := rowPath{ops: make([]format.Operation, 0, 4)}

	n, err := p.decode(enc)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the UPDATE carries one operation")
	require.Equal(t, uint32(1), p.body.IndexID)

	allocs := testing.AllocsPerRun(100, func() {
		if _, err := p.decode(enc); err != nil {
			panic(err)
		}
	})
	assert.Zero(t, allocs, "header + body + operations allocs")

	buf := make([]byte, 0, len(enc))

	allocs = testing.AllocsPerRun(100, func() {
		buf, _ = format.EncodeXRow(buf[:0], &p.row)
	})
	assert.Zero(t, allocs, "EncodeXRow into a reused buffer allocs")
	assert.Equal(t, enc, buf, "the decoded row encodes back to the same bytes")
}

// TestRowPath_ZeroAlloc_Fixtures runs the row path over every row of the
// historical xlogs and snapshots, written by real Tarantools: once the
// destinations have grown to the largest row, decoding a whole file
// allocates nothing.
func TestRowPath_ZeroAlloc_Fixtures(t *testing.T) { //nolint:paralleltest // AllocsPerRun counts process-wide mallocs
	if testing.Short() {
		t.Skip("allocation counts: skipped in -short mode")
	}

	var files []string

	for _, pattern := range []string{"*.xlog", "*.snap"} {
		m, err := filepath.Glob(filepath.Join("..", "testdata", "historical", "*", pattern))
		require.NoError(t, err)

		files = append(files, m...)
	}

	require.NotEmpty(t, files)

	for _, file := range files {
		rows := fileRowBytes(t, file)

		var (
			p   rowPath
			ops int
		)

		for _, b := range rows {
			n, err := p.decode(b)
			require.NoError(t, err, file)

			ops += n
		}

		allocs := testing.AllocsPerRun(10, func() {
			for _, b := range rows {
				if _, err := p.decode(b); err != nil {
					panic(err)
				}
			}
		})
		assert.Zerof(t, allocs, "%s: %d rows, %d operations: allocs", file, len(rows), ops)
	}
}

// fileRowBytes returns the bytes of every row of the xlog-envelope file at
// path, decompressed and copied out of their tx blocks.
func fileRowBytes(t *testing.T, path string) [][]byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = format.DecodeMeta(bufio.NewReader(bytes.NewReader(data)), format.MetaOptions{})
	require.NoError(t, err)

	off := bytes.Index(data, []byte("\n\n")) + 2

	var rows [][]byte

	for off < len(data) && !bytes.HasPrefix(data[off:], format.EOFMarker[:]) {
		slices, _, n, err := format.DecodeTxBlock(data[off:])
		require.NoError(t, err, path)

		for _, s := range slices {
			rows = append(rows, append([]byte(nil), s...))
		}

		off += n
	}

	return rows
}
