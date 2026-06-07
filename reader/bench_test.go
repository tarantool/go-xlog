package reader_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// benchBody returns a valid msgpack bin value of n bytes.
func benchBody(n int) []byte {
	b := make([]byte, 0, n+2)
	b = append(b, 0xc4, byte(n)) // 0xc4 = msgpack bin8.
	b = append(b, make([]byte, n)...)

	return b
}

// benchNtx is the single-row tx count every reader benchmark builds its log
// from (large enough that per-row work dominates per-op construction).
const benchNtx = 1000

// buildBenchLog produces an in-memory xlog with benchNtx single-row txs.
func buildBenchLog(b *testing.B) []byte {
	b.Helper()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, &format.Meta{
		Filetype: format.FiletypeXLOG,
		Version:  "go-xlog/bench",
		VClock:   format.VClock{1: 0},
	})
	if err != nil {
		b.Fatal(err)
	}

	for i := range benchNtx {
		row := format.XRow{Type: iproto.IPROTO_INSERT, LSN: int64(i + 1), BodyRaw: benchBody(64)}
		if err := w.WriteTx([]format.XRow{row}); err != nil {
			b.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		b.Fatal(err)
	}

	return buf.Bytes()
}

// BenchmarkReaderNext drains the whole log via the legacy Next() API once per
// op. Allocs/op is dominated by the per-row cost for a large log.
func BenchmarkReaderNext(b *testing.B) {
	data := buildBenchLog(b)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		r, err := reader.NewReader(bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}

		for {
			_, err := r.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				b.Fatal(err)
			}
		}

		_ = r.Close()
	}
}

// BenchmarkReaderScan drains the whole log via the zero-alloc Scan cursor,
// calling Recycle each op so the arena memory is bounded (streaming shape).
func BenchmarkReaderScan(b *testing.B) {
	data := buildBenchLog(b)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		r, err := reader.NewReader(bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}

		for r.Scan() {
			_ = r.Row()
		}

		if err := r.Err(); err != nil {
			b.Fatal(err)
		}

		r.Recycle()
		_ = r.Close()
	}
}

// BenchmarkReaderScanAlias is Scan with WithAliasBodies — no per-row body copy.
func BenchmarkReaderScanAlias(b *testing.B) {
	data := buildBenchLog(b)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		r, err := reader.NewReader(bytes.NewReader(data), reader.WithAliasBodies())
		if err != nil {
			b.Fatal(err)
		}

		for r.Scan() {
			_ = r.Row()
		}

		if err := r.Err(); err != nil {
			b.Fatal(err)
		}

		r.Recycle()
		_ = r.Close()
	}
}

// drainScan loops a reader's Scan cursor to completion, failing on error.
func drainScan(b *testing.B, r *reader.Reader) {
	b.Helper()

	for r.Scan() {
		_ = r.Row()
	}

	if err := r.Err(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkReaderStreamBytes contrasts the three ways to read a fully in-memory
// log with the alias-body Scan cursor: the streaming reader over a bytes.Reader
// (copies each block through bufio into txBuf) versus the in-memory reader
// (NewReaderBytes — slices blocks straight out of the buffer, no copy). It
// isolates the per-block memmove the in-memory path removes.
func BenchmarkReaderStreamBytes(b *testing.B) {
	data := buildBenchLog(b)

	b.Run("stream", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		b.ResetTimer()

		for range b.N {
			r, err := reader.NewReader(bytes.NewReader(data), reader.WithAliasBodies())
			if err != nil {
				b.Fatal(err)
			}

			drainScan(b, r)
			_ = r.Close()
		}
	})

	b.Run("bytes", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		b.ResetTimer()

		for range b.N {
			r, err := reader.NewReaderBytes(data, reader.WithAliasBodies())
			if err != nil {
				b.Fatal(err)
			}

			drainScan(b, r)
			_ = r.Close()
		}
	})
}

// BenchmarkReaderOpenMmap contrasts Open (buffered file I/O) with OpenMmap
// (memory-mapped, zero-copy block slicing) over the same on-disk log, draining
// with the alias-body Scan cursor.
func BenchmarkReaderOpenMmap(b *testing.B) {
	data := buildBenchLog(b)

	dir := b.TempDir()
	path := filepath.Join(dir, "bench.xlog")

	if err := os.WriteFile(path, data, 0o600); err != nil {
		b.Fatal(err)
	}

	b.Run("open", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		b.ResetTimer()

		for range b.N {
			r, err := reader.Open(path, reader.WithAliasBodies())
			if err != nil {
				b.Fatal(err)
			}

			drainScan(b, r)
			_ = r.Close()
		}
	})

	b.Run("mmap", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		b.ResetTimer()

		for range b.N {
			r, err := reader.OpenMmap(path, reader.WithAliasBodies())
			if err != nil {
				b.Fatal(err)
			}

			drainScan(b, r)
			_ = r.Close()
		}
	})
}
