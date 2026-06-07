package format_test

import (
	"fmt"
	"hash/crc32"
	"testing"
)

var benchCastagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32cHardware is the candidate replacement: the wire CRC (init=0, no xorout)
// recovered from Go's hardware-accelerated Castagnoli path via the
// length-independent framing identity.
func crc32cHardware(data []byte) uint32 {
	return crc32.Update(^uint32(0), benchCastagnoli, data) ^ ^uint32(0)
}

// crc32cSoftware is the current production implementation: a byte-at-a-time
// table loop (init=0, no xorout). Duplicated here so the benchmark compares the
// two directly without depending on which one format.CRC32C currently is.
func crc32cSoftware(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = benchCastagnoli[byte(crc)^b] ^ (crc >> 8)
	}

	return crc
}

func benchmarkCRC(b *testing.B, size int, fn func([]byte) uint32) {
	b.Helper()

	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}

	// Sanity: the two implementations must agree on this input.
	if crc32cSoftware(data) != crc32cHardware(data) {
		b.Fatal("software/hardware CRC disagree")
	}

	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()

	var sink uint32
	for range b.N {
		sink = fn(data)
	}

	runtimeSink = sink
}

var runtimeSink uint32

// BenchmarkCRC32C compares the byte-at-a-time loop against the hardware path
// across payload sizes: a fixheader (19 B), a small row (64 B), the compression
// threshold (2 KiB), and large blocks (64 KiB, 256 KiB).
func BenchmarkCRC32C(b *testing.B) {
	for _, sz := range []int{19, 64, 2048, 65536, 262144} {
		b.Run(fmt.Sprintf("software/%d", sz), func(b *testing.B) { benchmarkCRC(b, sz, crc32cSoftware) })
		b.Run(fmt.Sprintf("hardware/%d", sz), func(b *testing.B) { benchmarkCRC(b, sz, crc32cHardware) })
	}
}
