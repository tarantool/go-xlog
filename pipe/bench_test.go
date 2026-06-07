package pipe_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/pipe"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// buildBatchedSource writes ntx single-row txs packed 50-per-block via a
// BatchWriter, so the blocks cross the compression threshold and land as ZRow
// (compressed) blocks — the shape Tarantool's own xlog uses. This is the source
// both repack benchmarks consume.
func buildBatchedSource(b *testing.B, ntx int) []byte {
	b.Helper()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, newBenchMeta())
	if err != nil {
		b.Fatal(err)
	}

	bw := writer.NewBatchWriter(w, writer.BatchOptions{MaxTxs: 50})

	body := make([]uint64, 8) // ~ a couple dozen bytes per row.

	for i := range ntx {
		body[0] = uint64(i)

		row := format.XRow{Type: iproto.IPROTO_INSERT, LSN: int64(i + 1), BodyRaw: dmlBody(body...)}
		if err := bw.WriteTx([]format.XRow{row}); err != nil {
			b.Fatal(err)
		}
	}

	if err := bw.Close(); err != nil {
		b.Fatal(err)
	}

	return buf.Bytes()
}

func newBenchMeta() *format.Meta {
	return &format.Meta{Filetype: format.FiletypeXLOG, Version: "go-xlog/bench", VClock: format.VClock{1: 0}}
}

func mustDiscardWriter(b *testing.B) *writer.Writer {
	b.Helper()

	w, err := writer.NewWriter(io.Discard, newBenchMeta())
	if err != nil {
		b.Fatal(err)
	}

	return w
}

// BenchmarkCopyRaw — verbatim block-copy repack: NextBlockRaw -> WriteRawBlock.
// No row decode, no re-encode, no recompression, no second CRC. The source's
// compressed blocks are forwarded byte-for-byte.
func BenchmarkCopyRaw(b *testing.B) {
	for _, ntx := range []int{1000, 100_000} {
		b.Run(fmt.Sprintf("ntx=%d", ntx), func(b *testing.B) {
			src := buildBatchedSource(b, ntx)

			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			b.ResetTimer()

			for range b.N {
				rd, err := reader.NewReader(bytes.NewReader(src))
				if err != nil {
					b.Fatal(err)
				}

				if _, err := pipe.CopyRaw(rd, mustDiscardWriter(b)); err != nil {
					b.Fatal(err)
				}

				_ = rd.Close()
			}
		})
	}
}

// BenchmarkRepackRows — the row-based repack over the same source: Scan-alias
// every row and re-emit through a BatchWriter into fresh compressed blocks. This
// pays the full decode + re-encode + decompress + recompress + second CRC that
// CopyRaw elides, so the two together quantify the verbatim win.
func BenchmarkRepackRows(b *testing.B) {
	for _, ntx := range []int{1000, 100_000} {
		b.Run(fmt.Sprintf("ntx=%d", ntx), func(b *testing.B) {
			src := buildBatchedSource(b, ntx)
			txbuf := make([]format.XRow, 1)

			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			b.ResetTimer()

			for range b.N {
				rd, err := reader.NewReader(bytes.NewReader(src), reader.WithAliasBodies())
				if err != nil {
					b.Fatal(err)
				}

				bw := writer.NewBatchWriter(mustDiscardWriter(b), writer.BatchOptions{MaxTxs: 50})

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
		})
	}
}
