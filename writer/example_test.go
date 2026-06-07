package writer_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

func exampleMeta() *format.Meta {
	return &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "example",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
	}
}

// exampleBody msgpack-encodes a one-element tuple so the row body is a valid
// msgpack value the reader can round-trip.
func exampleBody(v int) []byte {
	b, err := msgpack.Marshal([]int{v})
	if err != nil {
		panic(err)
	}

	return b
}

// ExampleWriter writes a couple of single-statement transactions to an
// in-memory xlog, then reads them back.
func ExampleWriter() {
	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, exampleMeta())
	if err != nil {
		panic(err)
	}

	for lsn := int64(1); lsn <= 2; lsn++ {
		if err := w.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: exampleBody(int(lsn))},
		}); err != nil {
			panic(err)
		}
	}

	if err := w.Close(); err != nil {
		panic(err)
	}

	r, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	count := 0

	for _, err := range r.Rows() {
		if err != nil {
			panic(err)
		}

		count++
	}

	fmt.Printf("read back %d record(s)\n", count)

	// Output:
	// read back 2 record(s)
}

// ExampleBatchWriter packs many independent single-row transactions into
// compressed blocks. Each transaction keeps its own identity (it stays a
// single-row autocommit tx) while sharing a physical block, which is the
// shape Tarantool's own xlog uses.
func ExampleBatchWriter() {
	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, exampleMeta())
	if err != nil {
		panic(err)
	}

	// Flush a block every two transactions (a real caller would use a larger
	// MaxBytes so blocks cross the zstd compression threshold).
	bw := writer.NewBatchWriter(w, writer.BatchOptions{MaxTxs: 2})

	for lsn := int64(1); lsn <= 5; lsn++ {
		if err := bw.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: exampleBody(int(lsn))},
		}); err != nil {
			panic(err)
		}
	}

	if err := bw.Close(); err != nil { // Flushes the trailing block + closes.
		panic(err)
	}

	r, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	txs, multiRow := 0, 0

	for tx, err := range r.Txs() {
		if err != nil {
			panic(err)
		}

		txs++

		if len(tx.Rows) != 1 {
			multiRow++
		}
	}

	fmt.Printf("%d transactions preserved, %d merged\n", txs, multiRow)

	// Output:
	// 5 transactions preserved, 0 merged
}

// ExampleWriter_WriteBlock copies records from one xlog into a single verbatim
// block of another. WriteBlock writes the rows as-is — it trusts the TSN/commit
// flags they already carry (here, from the source reader) — so it is the
// efficient primitive for copy/repack.
func ExampleWriter_WriteBlock() {
	// A small source xlog to copy from.
	var src bytes.Buffer

	sw, err := writer.NewWriter(&src, exampleMeta())
	if err != nil {
		panic(err)
	}

	for lsn := int64(1); lsn <= 3; lsn++ {
		if err := sw.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: exampleBody(int(lsn))},
		}); err != nil {
			panic(err)
		}
	}

	if err := sw.Close(); err != nil {
		panic(err)
	}

	// Read every record, cloning it (BodyRaw aliases the reader's buffer).
	sr, err := reader.NewReader(bytes.NewReader(src.Bytes()))
	if err != nil {
		panic(err)
	}

	var block []format.XRow

	for row, err := range sr.Rows() {
		if err != nil {
			panic(err)
		}

		clone := row
		clone.BodyRaw = append([]byte(nil), row.BodyRaw...)
		block = append(block, clone)
	}

	_ = sr.Close()

	// Write all of them into one block of a new xlog.
	var dst bytes.Buffer

	dw, err := writer.NewWriter(&dst, exampleMeta())
	if err != nil {
		panic(err)
	}

	if err := dw.WriteBlock(block); err != nil {
		panic(err)
	}

	if err := dw.Close(); err != nil {
		panic(err)
	}

	fmt.Printf("copied %d records into one block\n", len(block))

	// Output:
	// copied 3 records into one block
}

// ExampleBatchWriter_pipeline streams rows straight from a reader into a
// BatchWriter with no cloning — the caching-dumper repack path. Because
// BatchWriter encodes each transaction eagerly into its pending buffer, it
// never retains the caller's rows, so rows whose BodyRaw aliases the reader's
// recycled read buffer (reader.WithAliasBodies, the fastest read mode) can be
// forwarded directly.
func ExampleBatchWriter_pipeline() {
	// Build a small source xlog to repack.
	var srcBuf bytes.Buffer

	sw, err := writer.NewWriter(&srcBuf, exampleMeta())
	if err != nil {
		panic(err)
	}

	for lsn := int64(1); lsn <= 5; lsn++ {
		if err := sw.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: exampleBody(int(lsn))},
		}); err != nil {
			panic(err)
		}
	}

	if err := sw.Close(); err != nil {
		panic(err)
	}

	// Repack: read with aliased bodies, forward each row to the BatchWriter
	// without cloning.
	rd, err := reader.NewReader(bytes.NewReader(srcBuf.Bytes()), reader.WithAliasBodies())
	if err != nil {
		panic(err)
	}

	defer func() { _ = rd.Close() }()

	var dstBuf bytes.Buffer

	dw, err := writer.NewWriter(&dstBuf, exampleMeta())
	if err != nil {
		panic(err)
	}

	bw := writer.NewBatchWriter(dw, writer.BatchOptions{MaxTxs: 2})

	for rd.Scan() {
		row := rd.Row() // BodyRaw aliases the read buffer — no clone needed.
		if err := bw.WriteTx([]format.XRow{row}); err != nil {
			panic(err)
		}
	}

	if err := rd.Err(); err != nil {
		panic(err)
	}

	if err := bw.Close(); err != nil {
		panic(err)
	}

	// Read the repacked output back to confirm every record survived.
	out, err := reader.NewReader(bytes.NewReader(dstBuf.Bytes()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = out.Close() }()

	count := 0

	for _, err := range out.Rows() {
		if err != nil {
			panic(err)
		}

		count++
	}

	fmt.Printf("repacked %d records (no clone)\n", count)
	// Output:
	// repacked 5 records (no clone)
}

// ExampleWriter_WriteRawBlock forwards physical tx blocks verbatim: each block's
// on-disk bytes are read with reader.NextBlockRaw and written unchanged with
// WriteRawBlock — no row decode, no re-encode, no recompression, no CRC
// recompute. This is the manual form of the fast copy path that pipe.CopyRaw
// wraps; reach for it when a tool needs to interleave verbatim block forwarding
// with other logic. A compressed block stays compressed on disk.
func ExampleWriter_WriteRawBlock() {
	// A small source xlog to copy from.
	var srcBuf bytes.Buffer

	sw, err := writer.NewWriter(&srcBuf, exampleMeta())
	if err != nil {
		panic(err)
	}

	for lsn := int64(1); lsn <= 3; lsn++ {
		if err := sw.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: exampleBody(int(lsn))},
		}); err != nil {
			panic(err)
		}
	}

	if err := sw.Close(); err != nil {
		panic(err)
	}

	rd, err := reader.NewReader(bytes.NewReader(srcBuf.Bytes()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = rd.Close() }()

	var dstBuf bytes.Buffer

	dw, err := writer.NewWriter(&dstBuf, exampleMeta())
	if err != nil {
		panic(err)
	}

	blocks := 0

	for {
		block, err := rd.NextBlockRaw()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			panic(err)
		}

		// Block is valid only until the next NextBlockRaw call; WriteRawBlock
		// consumes it immediately, so no copy is needed.
		if err := dw.WriteRawBlock(block); err != nil {
			panic(err)
		}

		blocks++
	}

	if err := dw.Close(); err != nil {
		panic(err)
	}

	fmt.Printf("forwarded %d blocks verbatim\n", blocks)

	// Output:
	// forwarded 3 blocks verbatim
}
