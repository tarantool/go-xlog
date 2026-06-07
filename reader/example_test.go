package reader_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// sampleXlog builds a small in-memory xlog: one two-row transaction
// (LSN 1, 2) followed by one single-row transaction (LSN 3). It is used by
// the reader examples so they are self-contained.
func sampleXlog() []byte {
	meta := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "example",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
	}

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, meta)
	if err != nil {
		panic(err)
	}

	body := func(v int) []byte {
		b, err := msgpack.Marshal([]int{v})
		if err != nil {
			panic(err)
		}

		return b
	}

	if err := w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: body(1)},
		{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: body(2)},
	}); err != nil {
		panic(err)
	}

	if err := w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_REPLACE, LSN: 3, BodyRaw: body(3)},
	}); err != nil {
		panic(err)
	}

	if err := w.Close(); err != nil {
		panic(err)
	}

	return buf.Bytes()
}

// ExampleReader_Rows reads an xlog record by record, with no awareness of
// transaction boundaries. Rows yields every xrow in stream order, crossing
// (and decompressing) tx blocks transparently.
func ExampleReader_Rows() {
	r, err := reader.NewReader(bytes.NewReader(sampleXlog()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	for row, err := range r.Rows() {
		if err != nil {
			panic(err)
		}

		fmt.Printf("record: type=%d lsn=%d\n", row.Type, row.LSN)
	}

	// Output:
	// record: type=2 lsn=1
	// record: type=2 lsn=2
	// record: type=3 lsn=3
}

// ExampleReader_Txs reads the same xlog grouped into logical transactions.
// The reader reconstructs transactions from each row's TSN/commit flags, so
// the two-row tx and the single-row tx come back distinct.
func ExampleReader_Txs() {
	r, err := reader.NewReader(bytes.NewReader(sampleXlog()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	for tx, err := range r.Txs() {
		if err != nil {
			panic(err)
		}

		fmt.Printf("transaction: %d row(s), startLSN=%d\n", len(tx.Rows), tx.StartLSN)
	}

	// Output:
	// transaction: 2 row(s), startLSN=1
	// transaction: 1 row(s), startLSN=3
}

// ExampleReadHeader reads only the meta header of an xlog (filetype, version,
// instance UUID, vclock) without scanning any tx blocks. It is the one-shot
// convenience for "what is this file?" inspection.
func ExampleReadHeader() {
	// Stage the sample bytes to a file, since ReadHeader takes a path.
	path := filepath.Join(os.TempDir(), "go-xlog-example.xlog")
	if err := os.WriteFile(path, sampleXlog(), 0o600); err != nil {
		panic(err)
	}

	defer func() { _ = os.Remove(path) }()

	m, err := reader.ReadHeader(path)
	if err != nil {
		panic(err)
	}

	fmt.Printf("filetype=%s version=%s uuid=%s\n", m.Filetype, m.Version, m.InstanceUUID)

	// Output:
	// filetype=XLOG version=example uuid=11111111-2222-3333-4444-555555555555
}

// ExampleReader_Scan reads record by record through the zero-allocation cursor.
// Unlike Rows/Next, Scan decodes each row into reader-owned scratch, so a tight
// loop performs no per-row heap allocation. Check Err after the loop; Row() is
// valid until the next Scan (and safe to retain until Recycle — see
// ExampleReader_Recycle).
func ExampleReader_Scan() {
	r, err := reader.NewReader(bytes.NewReader(sampleXlog()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	for r.Scan() {
		row := r.Row()
		fmt.Printf("record: type=%d lsn=%d\n", row.Type, row.LSN)
	}

	if err := r.Err(); err != nil {
		panic(err)
	}

	// Output:
	// record: type=2 lsn=1
	// record: type=2 lsn=2
	// record: type=3 lsn=3
}

// ExampleReader_ScanTx reads transactions through the zero-allocation cursor.
// ScanTx groups rows by their TSN/commit flags exactly like Txs, but decodes
// into reusable scratch; Tx() returns the rows of the current transaction.
func ExampleReader_ScanTx() {
	r, err := reader.NewReader(bytes.NewReader(sampleXlog()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	for r.ScanTx() {
		rows := r.Tx()
		fmt.Printf("transaction: %d row(s), startLSN=%d\n", len(rows), rows[0].LSN)
	}

	if err := r.Err(); err != nil {
		panic(err)
	}

	// Output:
	// transaction: 2 row(s), startLSN=1
	// transaction: 1 row(s), startLSN=3
}

// ExampleReader_Recycle shows the retain-and-recycle pattern. Rows returned by
// Scan are safe to retain until Recycle, so a caller can accumulate a batch,
// process it, then Recycle to reclaim the arena memory before the next batch —
// bounded memory while still holding many rows at once. After Recycle the
// previously returned rows (and their BodyRaw) must not be touched.
func ExampleReader_Recycle() {
	r, err := reader.NewReader(bytes.NewReader(sampleXlog()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	const batchSize = 2

	var batch []format.XRow

	flush := func() {
		if len(batch) == 0 {
			return
		}

		fmt.Printf("batch of %d (lsn %d..%d)\n", len(batch), batch[0].LSN, batch[len(batch)-1].LSN)

		batch = batch[:0]

		r.Recycle() // Reclaim the arena; rows above must not be used after this.
	}

	for r.Scan() {
		batch = append(batch, r.Row()) // Safe to retain until Recycle.
		if len(batch) == batchSize {
			flush()
		}
	}

	if err := r.Err(); err != nil {
		panic(err)
	}

	flush() // Trailing partial batch.

	// Output:
	// batch of 2 (lsn 1..2)
	// batch of 1 (lsn 3..3)
}

// ExampleWithAliasBodies is the fastest read path: for streaming consumers that
// fully process each row before advancing, WithAliasBodies skips the per-row
// body copy, so Row().BodyRaw aliases the read buffer and is valid only until
// the next Scan. Do not retain rows (or their BodyRaw) past the next Scan.
func ExampleWithAliasBodies() {
	r, err := reader.NewReader(bytes.NewReader(sampleXlog()), reader.WithAliasBodies())
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	total := 0

	for r.Scan() {
		total += len(r.Row().BodyRaw) // Consumed now, not retained past the next Scan.
	}

	if err := r.Err(); err != nil {
		panic(err)
	}

	fmt.Printf("scanned 3 records, total body bytes=%d\n", total)

	// Output:
	// scanned 3 records, total body bytes=6
}

// ExampleNewReaderBytes reads a journal that is already fully in memory
// (preloaded, embedded, or memory-mapped). Instead of copying each tx block
// through an internal buffer, the reader slices blocks directly out of the
// input — and for uncompressed blocks the decoded row bodies alias that buffer.
// Combined with WithAliasBodies this is end-to-end zero-copy, and because the
// input outlives the reader the bodies stay valid even after the reader
// advances (unlike the streaming WithAliasBodies, where bodies last only until
// the next Scan).
func ExampleNewReaderBytes() {
	data := sampleXlog() // A complete journal image already in memory.

	r, err := reader.NewReaderBytes(data, reader.WithAliasBodies())
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	// Retain every row through the whole scan: with NewReaderBytes the aliased
	// bodies remain valid because they point into data, which we still hold.
	var rows []format.XRow
	for r.Scan() {
		rows = append(rows, r.Row())
	}

	if err := r.Err(); err != nil {
		panic(err)
	}

	for _, row := range rows {
		fmt.Printf("record: type=%d lsn=%d body=%d bytes\n", row.Type, row.LSN, len(row.BodyRaw))
	}

	// Output:
	// record: type=2 lsn=1 body=2 bytes
	// record: type=2 lsn=2 body=2 bytes
	// record: type=3 lsn=3 body=2 bytes
}

// ExampleOpenMmap memory-maps a journal file and reads it with no per-block
// copies or read syscalls in the steady state. It is the path-shaped
// counterpart to NewReaderBytes; Close unmaps the file. On platforms without
// mmap it transparently falls back to reading the whole file into memory.
func ExampleOpenMmap() {
	// Stage the sample bytes to a file, since OpenMmap takes a path.
	path := filepath.Join(os.TempDir(), "go-xlog-mmap-example.xlog")
	if err := os.WriteFile(path, sampleXlog(), 0o600); err != nil {
		panic(err)
	}

	defer func() { _ = os.Remove(path) }()

	r, err := reader.OpenMmap(path)
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }() // Unmaps the file.

	count := 0

	for r.Scan() {
		count++
	}

	if err := r.Err(); err != nil {
		panic(err)
	}

	fmt.Printf("read %d records via mmap\n", count)

	// Output:
	// read 3 records via mmap
}

// ExampleReader_NextBlockRaw walks a journal one physical tx block at a time
// without decoding any rows. NextBlockRaw returns each block's on-disk bytes
// (fixheader + payload) verbatim, CRC-verified — the read half of the
// verbatim-copy fast path (see pipe.CopyRaw). It is useful for tooling that
// counts, indexes, or forwards blocks without paying for row decoding.
func ExampleReader_NextBlockRaw() {
	r, err := reader.NewReader(bytes.NewReader(sampleXlog()))
	if err != nil {
		panic(err)
	}

	defer func() { _ = r.Close() }()

	blocks := 0

	for {
		block, err := r.NextBlockRaw()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			panic(err)
		}

		_ = block // The verbatim fixheader+payload, ready to forward as-is.
		blocks++
	}

	// The sample has two transactions, so two physical blocks.
	fmt.Printf("%d physical blocks\n", blocks)

	// Output:
	// 2 physical blocks
}
