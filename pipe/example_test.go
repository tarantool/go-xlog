package pipe_test

import (
	"bytes"
	"fmt"

	"github.com/google/uuid"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/filter"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/pipe"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// ExampleCopy streams transactions from a source xlog to a destination,
// keeping only those that pass a filter. Here it copies INSERT rows and drops
// the REPLACE. Filtering is per transaction: if any row in a tx matches, the
// whole tx is written.
func ExampleCopy() {
	meta := func() *format.Meta {
		return &format.Meta{
			Filetype:     format.FiletypeXLOG,
			Version:      "example",
			InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		}
	}

	body := func(v int) []byte {
		b, err := msgpack.Marshal([]int{v})
		if err != nil {
			panic(err)
		}

		return b
	}

	// Build a source with three single-row txs: INSERT, REPLACE, INSERT.
	var srcBuf bytes.Buffer

	sw, err := writer.NewWriter(&srcBuf, meta())
	if err != nil {
		panic(err)
	}

	for _, row := range []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: body(1)},
		{Type: iproto.IPROTO_REPLACE, LSN: 2, BodyRaw: body(2)},
		{Type: iproto.IPROTO_INSERT, LSN: 3, BodyRaw: body(3)},
	} {
		if err := sw.WriteTx([]format.XRow{row}); err != nil {
			panic(err)
		}
	}

	if err := sw.Close(); err != nil {
		panic(err)
	}

	// Copy INSERTs only.
	src, err := reader.NewReader(bytes.NewReader(srcBuf.Bytes()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = src.Close() }()

	var dstBuf bytes.Buffer

	dst, err := writer.NewWriter(&dstBuf, meta())
	if err != nil {
		panic(err)
	}

	n, err := pipe.Copy(src, dst, filter.Types(iproto.IPROTO_INSERT))
	if err != nil {
		panic(err)
	}

	if err := dst.Close(); err != nil {
		panic(err)
	}

	fmt.Printf("copied %d of 3 records\n", n)

	// Output:
	// copied 2 of 3 records
}

// ExampleCopyRaw copies a journal verbatim, forwarding each physical tx block's
// on-disk bytes without decoding rows, re-encoding, recompressing, or
// recomputing CRCs. It is the fast path for a pure copy/truncate where rows are
// not transformed: a compressed block stays compressed on disk. Unlike Copy it
// cannot filter (that needs row-level decoding).
func ExampleCopyRaw() {
	meta := func() *format.Meta {
		return &format.Meta{
			Filetype:     format.FiletypeXLOG,
			Version:      "example",
			InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		}
	}

	body := func(v int) []byte {
		b, err := msgpack.Marshal([]int{v})
		if err != nil {
			panic(err)
		}

		return b
	}

	// Build a source with three single-row transactions.
	var srcBuf bytes.Buffer

	sw, err := writer.NewWriter(&srcBuf, meta())
	if err != nil {
		panic(err)
	}

	for lsn := 1; lsn <= 3; lsn++ {
		if err := sw.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: int64(lsn), BodyRaw: body(lsn)}}); err != nil {
			panic(err)
		}
	}

	if err := sw.Close(); err != nil {
		panic(err)
	}

	// Forward every block verbatim into a new journal.
	src, err := reader.NewReader(bytes.NewReader(srcBuf.Bytes()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = src.Close() }()

	var dstBuf bytes.Buffer

	dst, err := writer.NewWriter(&dstBuf, meta())
	if err != nil {
		panic(err)
	}

	blocks, err := pipe.CopyRaw(src, dst)
	if err != nil {
		panic(err)
	}

	if err := dst.Close(); err != nil {
		panic(err)
	}

	fmt.Printf("copied %d blocks verbatim\n", blocks)

	// Output:
	// copied 3 blocks verbatim
}
