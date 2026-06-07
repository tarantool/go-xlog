package format_test

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
)

// TestCRC32C_MatchesReference pins the hardware-accelerated CRC32C against the
// byte-at-a-time software reference (crc32cSoftware, in crc32c_bench_test.go)
// over fixed-size and random inputs — the length-independent framing identity
// must hold for every length and content.
func TestCRC32C_MatchesReference(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(1))

	for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 15, 16, 31, 63, 64, 127, 255, 256, 1000, 4096, 65537} {
		buf := make([]byte, n)
		_, _ = rng.Read(buf)
		require.Equalf(t, crc32cSoftware(buf), format.CRC32C(buf), "random len=%d", n)

		zeros := make([]byte, n)
		require.Equalf(t, crc32cSoftware(zeros), format.CRC32C(zeros), "zeros len=%d", n)

		ffs := bytes.Repeat([]byte{0xff}, n)
		require.Equalf(t, crc32cSoftware(ffs), format.CRC32C(ffs), "0xff len=%d", n)
	}
}

// TestCRC32C_Vector_SimpleXlog walks the first tx block in simple.xlog,
// extracts the on-disk fixheader's stated CRC32C, recomputes it over the
// payload bytes, and asserts the two match. This is the highest-value
// vector test: it ties our CRC32C implementation directly to a real
// Tarantool-produced file.
func TestCRC32C_Vector_SimpleXlog(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	// Meta header ends at the first 0x0a 0x0a (blank-line terminator).
	off := indexBlankLine(t, data)
	require.Equalf(t, format.RowMarker[0], data[off], "expected RowMarker at offset %d, got 0x%02x", off, data[off])

	var fh [format.FixheaderSize]byte
	copy(fh[:], data[off:off+format.FixheaderSize])
	h, err := format.DecodeFixheader(fh)
	require.NoError(t, err)

	payload := data[off+format.FixheaderSize : off+format.FixheaderSize+int(h.Len)]
	got := format.CRC32C(payload)
	require.Equalf(t, h.CRC32C, got, "CRC mismatch (Len=%d)", h.Len)
}

// TestCRC32C_NotStdlibChecksum confirms we are using the variant — that is,
// CRC32C of empty bytes is 0 (init=0, no final XOR) rather than 0xffffffff.
func TestCRC32C_EmptyIsZero(t *testing.T) {
	t.Parallel()

	require.Equalf(t, uint32(0), format.CRC32C(nil), "CRC32C(nil) should be 0 (init=0, no final XOR)")
	require.Equalf(t, uint32(0), format.CRC32C([]byte{}), "CRC32C([]byte{}) should be 0")
}

// indexBlankLine returns the offset just after the meta-header blank-line
// terminator ("\n\n").
func indexBlankLine(t *testing.T, data []byte) int {
	t.Helper()

	for i := 0; i+1 < len(data); i++ {
		if data[i] == '\n' && data[i+1] == '\n' {
			return i + 2
		}
	}

	require.Fail(t, "meta header blank-line terminator not found")

	return -1
}
