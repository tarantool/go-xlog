package writer //nolint:testpackage // reuses internal encodeDMLBody/newMeta helpers

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

func benchMeta() *format.Meta {
	return &format.Meta{Filetype: format.FiletypeXLOG, Version: "go-xlog/bench", VClock: format.VClock{1: 0}}
}

// benchWriter builds a Writer streaming into io.Discard (no fsync, no file
// lifecycle) so benchmarks measure only encode/buffer allocation.
func benchWriter(b *testing.B) *Writer {
	b.Helper()

	w, err := NewWriter(io.Discard, &format.Meta{
		Filetype: format.FiletypeXLOG,
		Version:  "go-xlog/bench",
		VClock:   format.VClock{1: 0},
	})
	if err != nil {
		b.Fatal(err)
	}

	return w
}

// BenchmarkWriterWriteTx — one single-row autocommit tx per op.
func BenchmarkWriterWriteTx(b *testing.B) {
	w := benchWriter(b)
	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1, 42})},
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if err := w.WriteTx(rows); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBatchWriter — feed single-row txs through a BatchWriter that packs
// them into compressed blocks (the caching-dumper shape).
func BenchmarkBatchWriter(b *testing.B) {
	w := benchWriter(b)
	bw := NewBatchWriter(w, BatchOptions{MaxTxs: 50})
	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1, 42})},
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if err := bw.WriteTx(rows); err != nil {
			b.Fatal(err)
		}
	}

	_ = bw.Flush()
}

// buildRepackSource builds an in-memory xlog of ntx single-row autocommit txs
// (each body ~bodyLen bytes) for the repack benchmarks.
func buildRepackSource(b *testing.B, ntx, bodyLen int) []byte {
	b.Helper()

	var buf bytes.Buffer

	w, err := NewWriter(&buf, benchMeta())
	if err != nil {
		b.Fatal(err)
	}

	for i := range ntx {
		row := format.XRow{Type: iproto.IPROTO_INSERT, LSN: int64(i + 1), BodyRaw: fixedDMLBody(512, bodyLen)}
		if err := w.WriteTx([]format.XRow{row}); err != nil {
			b.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		b.Fatal(err)
	}

	return buf.Bytes()
}

// BenchmarkRepackBatch measures the caching-dumper repack: read every row from
// a source xlog with aliased bodies and re-emit it through a BatchWriter into
// compressed blocks, with no per-row cloning. This is the zero-alloc steady-
// state path. The ntx=1000 sub-benchmark still pays per-op reader/writer
// construction; the large sub-benchmark amortises it away so the profile
// reflects the true per-row work (CRC, zstd, msgpack, I/O).
func BenchmarkRepackBatch(b *testing.B) {
	for _, ntx := range []int{1000, 100_000} {
		b.Run(fmt.Sprintf("ntx=%d", ntx), func(b *testing.B) {
			benchRepackBatch(b, ntx, 64)
		})
	}
}

func benchRepackBatch(b *testing.B, ntx, bodyLen int) {
	b.Helper()

	src := buildRepackSource(b, ntx, bodyLen)
	txbuf := make([]format.XRow, 1) // Reused single-row tx; avoids a per-row slice alloc.

	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	b.ResetTimer()

	for range b.N {
		rd, err := reader.NewReader(bytes.NewReader(src), reader.WithAliasBodies())
		if err != nil {
			b.Fatal(err)
		}

		bw := NewBatchWriter(mustDiscardWriter(b), BatchOptions{MaxTxs: 50})

		for rd.Scan() {
			txbuf[0] = rd.Row()
			if err := bw.WriteTx(txbuf); err != nil {
				b.Fatal(err)
			}
		}

		if err := rd.Err(); err != nil {
			b.Fatal(err)
		}

		if err := bw.Close(); err != nil {
			b.Fatal(err)
		}

		_ = rd.Close()
	}
}

func mustDiscardWriter(b *testing.B) *Writer {
	b.Helper()

	w, err := NewWriter(io.Discard, benchMeta())
	if err != nil {
		b.Fatal(err)
	}

	return w
}
