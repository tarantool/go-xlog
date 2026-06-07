package format_test

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/tarantool/go-xlog/format"
)

// corpusProfiles are deterministic ~size-byte plain payloads spanning the data
// shapes a real tx block holds. "mixed" is the realistic xlog blend; the others
// isolate the extremes (text-heavy, zero-padded, incompressible) so the level
// sweep shows the speed/ratio tradeoff across data shapes, not one synthetic
// point. All are seeded, so results are stable run to run.
var corpusProfiles = []struct {
	name  string
	build func(size int) []byte
}{
	{"mixed", buildMixedPayload},
	{"text", buildTextPayload},
	{"zeros", buildZeroPayload},
	{"random", buildRandomPayload},
}

// dictWords stands in for the low-cardinality text a real log repeats: space
// names, index names, field keys. Drawn round-robin so they recur, as in real
// data.
var dictWords = []string{
	"users", "orders", "sessions", "primary", "secondary", "created_at",
	"updated_at", "email", "status", "amount", "currency", "tenant_id",
}

// buildMixedPayload is the realistic xlog blend: each row carries a near-
// identical msgpack-ish header prefix (highly compressible), a small
// incrementing integer, a recurring text field, a run of zero padding (nullable
// columns), and — for one row in sixteen — a 16-byte high-entropy blob
// (UUID/hash). The mixture compresses at a realistic ratio rather than an
// extreme.
func buildMixedPayload(size int) []byte {
	rng := rand.New(rand.NewSource(42))

	hdr := []byte{0x84, 0x00, 0x02, 0x02, 0x01, 0x03, 0xcd} // Pseudo xrow-header prefix.
	out := make([]byte, 0, size+64)

	for i := 0; len(out) < size; i++ {
		out = append(out, hdr...)
		out = append(out, byte(i), byte(i>>8), byte(i>>16)) // Incrementing id.

		word := dictWords[i%len(dictWords)]
		out = append(out, byte(0xa0|len(word))) // mp fixstr header.
		out = append(out, word...)

		out = append(out, make([]byte, 6)...) // Zero padding (nullable cols).

		if i%16 == 0 {
			var uuid [16]byte
			for j := range uuid {
				uuid[j] = byte(rng.Intn(256))
			}

			out = append(out, uuid[:]...)
		}
	}

	return out
}

// buildTextPayload is text-heavy: templated log-line-ish strings with varying
// numbers, drawing on the recurring dictionary. Stands for string-dominated
// spaces (JSON documents, names, messages).
func buildTextPayload(size int) []byte {
	out := make([]byte, 0, size+64)

	for i := 0; len(out) < size; i++ {
		line := fmt.Sprintf("row=%d space=%s field=%s value=%d ok\n",
			i, dictWords[i%len(dictWords)], dictWords[(i*7)%len(dictWords)], i*13%1000)
		out = append(out, line...)
	}

	return out
}

// buildZeroPayload is mostly zero padding with a sparse non-zero marker per
// row — the shape of sparse tuples with many nullable/default columns. Very
// compressible; the high-ratio end of the curve.
func buildZeroPayload(size int) []byte {
	out := make([]byte, 0, size+64)

	for i := 0; len(out) < size; i++ {
		out = append(out, make([]byte, 30)...)
		out = append(out, byte(i)) // One non-zero marker.
		out = append(out, byte(i>>8))
	}

	return out
}

// buildRandomPayload is incompressible seeded noise — the floor of the curve,
// where zstd spends cycles for ~no ratio.
func buildRandomPayload(size int) []byte {
	rng := rand.New(rand.NewSource(99))

	out := make([]byte, size)
	for i := range out {
		out[i] = byte(rng.Intn(256))
	}

	return out
}

// BenchmarkZstdLevels sweeps the zstd levels the writer can be configured with
// (Compression.Level) across each corpus profile, reporting ns/op, MB/s over the
// plain payload, and the achieved compression ratio + output size. It mirrors
// the writer hot path (CompressTxInto with a reused output buffer), so the
// speed/ratio tradeoff behind a Compression{Level} choice is explicit for every
// data shape.
func BenchmarkZstdLevels(b *testing.B) {
	const payloadBytes = 64 * 1024

	for _, profile := range corpusProfiles {
		plain := profile.build(payloadBytes)

		for _, level := range []int{1, 3, 9, 19} {
			b.Run(fmt.Sprintf("%s/level=%d", profile.name, level), func(b *testing.B) {
				dst := make([]byte, 0, len(plain))

				out, err := format.CompressTxInto(dst, plain, level)
				if err != nil {
					b.Fatal(err)
				}

				ratio := float64(len(plain)) / float64(len(out))

				b.ReportAllocs()
				b.SetBytes(int64(len(plain)))
				b.ResetTimer()

				for range b.N {
					out, err = format.CompressTxInto(dst, plain, level)
					if err != nil {
						b.Fatal(err)
					}
				}

				b.StopTimer()
				b.ReportMetric(ratio, "ratio")
				b.ReportMetric(float64(len(out)), "out-bytes")
			})
		}
	}
}

// BenchmarkZstdEncoderOptions compares, at the default level (3), the encoder
// the writer pool builds today against variants tuned with klauspost options:
// WithLowerEncoderMem (smaller per-encoder footprint) and a WithWindowSize
// capped at the max tx-block size (AutocommitThreshold). Tx blocks are bounded
// by AutocommitThreshold, so a larger window buys nothing but memory. This
// informs whether newZstdEncoder should adopt those options.
func BenchmarkZstdEncoderOptions(b *testing.B) {
	const payloadBytes = 64 * 1024

	plain := buildMixedPayload(payloadBytes)

	variants := []struct {
		name string
		opts []zstd.EOption
	}{
		{"default", []zstd.EOption{
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(format.ZstdLevel)),
			zstd.WithEncoderConcurrency(1),
		}},
		{"lowerMem", []zstd.EOption{
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(format.ZstdLevel)),
			zstd.WithEncoderConcurrency(1),
			zstd.WithLowerEncoderMem(true),
		}},
		{"window128K", []zstd.EOption{
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(format.ZstdLevel)),
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(format.AutocommitThreshold),
		}},
		{"lowerMem+window128K", []zstd.EOption{
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(format.ZstdLevel)),
			zstd.WithEncoderConcurrency(1),
			zstd.WithLowerEncoderMem(true),
			zstd.WithWindowSize(format.AutocommitThreshold),
		}},
	}

	for _, v := range variants {
		b.Run(v.name, func(b *testing.B) {
			enc, err := zstd.NewWriter(nil, v.opts...)
			if err != nil {
				b.Fatal(err)
			}

			defer func() { _ = enc.Close() }()

			dst := make([]byte, 0, len(plain))
			out := enc.EncodeAll(plain, dst[:0])
			ratio := float64(len(plain)) / float64(len(out))

			b.ReportAllocs()
			b.SetBytes(int64(len(plain)))
			b.ResetTimer()

			for range b.N {
				out = enc.EncodeAll(plain, dst[:0])
			}

			b.StopTimer()
			b.ReportMetric(ratio, "ratio")
			b.ReportMetric(float64(len(out)), "out-bytes")
		})
	}
}
