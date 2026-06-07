package format_test

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
)

// requireRowEqual asserts want == got, treating two NaN timestamps as equal
// (a NaN timestamp decodes deterministically but reflect.DeepEqual considers
// NaN != NaN, which would be a false mismatch in a differential test).
func requireRowEqual(t *testing.T, want, got format.XRow, msgAndArgs ...any) {
	t.Helper()

	if math.IsNaN(want.Timestamp) && math.IsNaN(got.Timestamp) {
		want.Timestamp, got.Timestamp = 0, 0
	}

	require.Equal(t, want, got, msgAndArgs...)
}

// The fuzz targets below assert one property: no decoder panics, hangs, or
// OOMs on arbitrary input — it returns an error or a value. A returned
// value is fine; we only require the decode call itself to be safe.

// fileSeeds adds the bytes of each testdata file (flat + historical corpus) as
// a fuzz seed, ignoring any that cannot be read.
func fileSeeds(f *testing.F) {
	f.Helper()

	roots := []string{
		filepath.Join("..", "testdata"),
		filepath.Join("..", "testdata", "historical"),
	}
	for _, root := range roots {
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil //nolint:nilerr // fuzz: malformed inputs are skipped, not failures
			}

			switch filepath.Ext(p) {
			case ".xlog", ".snap", ".vylog", ".run", ".index":
				if b, err := os.ReadFile(p); err == nil {
					f.Add(b)
				}
			}

			return nil
		})
	}
}

func FuzzDecodeMeta(f *testing.F) {
	fileSeeds(f)
	f.Add([]byte("XLOG\n0.13\n\n"))
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _ = format.DecodeMeta(bufio.NewReader(bytes.NewReader(b)), format.MetaOptions{AcceptV012: true})
	})
}

func FuzzDecodeXRow(f *testing.F) {
	f.Add([]byte{0x82, 0x00, 0x02, 0x10, 0x0c}) // {req_type:INSERT, space:12}-ish.
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _, _ = format.DecodeXRow(b)
	})
}

// FuzzDecodeXRowIntoReuse pins the decode-into core against the allocating
// wrapper while *reusing* one dst across a whole row stream — the scenario the
// reader runs and the field-bleed hazard (a stale field surviving into the
// next decode) that DecodeXRow, allocating fresh each call, can never expose.
// At every offset DecodeXRowInto into the reused dst must agree with a fresh
// DecodeXRow on both the consumed byte count and every field.
func FuzzDecodeXRowIntoReuse(f *testing.F) {
	f.Add([]byte{0x82, 0x00, 0x02, 0x10, 0x0c})
	f.Add([]byte{0x81, 0x00, 0x02, 0x80, 0x81, 0x00, 0x06, 0x80}) // Two tiny rows back-to-back.
	f.Add([]byte{})

	// Dst is deliberately reused across iterations and inputs.
	var dst format.XRow

	f.Fuzz(func(t *testing.T, b []byte) {
		off := 0
		for off < len(b) {
			want, wn, werr := format.DecodeXRow(b[off:])

			gn, gerr := format.DecodeXRowInto(b[off:], &dst)

			require.Equal(t, werr != nil, gerr != nil, "error disagreement at offset %d", off)

			if werr != nil {
				return
			}

			require.Equal(t, wn, gn, "consumed-bytes mismatch at offset %d", off)
			requireRowEqual(t, *want, dst, "decoded row mismatch at offset %d (field bleed?)", off)

			if gn == 0 {
				return // defensive: never advance zero (would loop)
			}

			off += gn
		}
	})
}

func FuzzDecodeTxBlock(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xd5, 0xba, 0x0b, 0xab}) // Bare row marker, no payload.
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _, _, _ = format.DecodeTxBlock(b)
	})
}

func FuzzDecodeDMLBody(f *testing.F) {
	f.Add([]byte{0x82, 0x10, 0x0c, 0x21, 0x91, 0x01}) // {space_id:12, tuple:[1]}.
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _ = format.DecodeDMLBody(b)
	})
}

func FuzzDecodeRaftBody(f *testing.F) {
	f.Add([]byte{0x81, 0x00, 0x02}) // {term:2}.
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _ = format.DecodeRaftBody(b)
	})
}

func FuzzDecodeSynchroBody(f *testing.F) {
	f.Add([]byte{0x82, 0x02, 0x01, 0x03, 0x05}) // {replica_id:1, lsn:5}.
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _ = format.DecodeSynchroBody(b)
	})
}

func FuzzDecodeVyLogBody(f *testing.F) {
	f.Add([]byte{0x81, 0x00, 0x01}) // {type:1}.
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _ = format.DecodeVyLogBody(b)
	})
}

func FuzzDecodeFixheader(f *testing.F) {
	// Seed with VALID-magic fixheaders. DecodeFixheaderInto rejects any prefix
	// that is not one of the three magic markers in its first lines, and only 3
	// of 2^32 four-byte values pass — which Go's mutator will essentially never
	// synthesise from all-zero seeds. Without these seeds the fuzzer bounces off
	// the magic gate forever (corpus stuck at the seed count, no new coverage)
	// and never exercises the mp_uint-width, len-overflow, or padding-shape
	// branches that follow. Varying the field magnitudes seeds the 1-/3-/5-byte
	// mp_uint encodings (and the zero-padding edge at 5+5+5 bytes).
	seed := func(magic [4]byte, length, crc32p, crc32c uint32) []byte {
		var buf [format.FixheaderSize]byte

		format.EncodeFixheader(&buf, &format.Fixheader{
			Magic: magic, Len: length, CRC32P: crc32p, CRC32C: crc32c,
		})

		return buf[:]
	}

	for _, m := range [][4]byte{format.RowMarker, format.ZRowMarker, format.EOFMarker} {
		f.Add(seed(m, 0, 0, 0))                                        // fixint widths.
		f.Add(seed(m, 0xff, 1, 2))                                     // uint8 widths.
		f.Add(seed(m, 0xffff, 0xabcd, 0x1234))                         // uint16 widths.
		f.Add(seed(m, math.MaxUint32, math.MaxUint32, math.MaxUint32)) // uint32 widths, zero padding.
	}

	f.Add(make([]byte, format.FixheaderSize)) // all-zero: the unknown-magic reject path
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		var arr [format.FixheaderSize]byte
		copy(arr[:], b) // Pad/truncate to the fixed size.

		h, err := format.DecodeFixheader(arr)
		if err != nil {
			return // A malformed header is rejected cleanly, never a panic.
		}

		// Beyond no-panic: a successfully decoded header must survive a re-encode
		// round trip. EncodeFixheader emits the canonical minimal-width form, so
		// the bytes may differ from arr, but decoding them again must reproduce
		// the identical struct — pinning the encode/decode pair as true inverses.
		var re [format.FixheaderSize]byte

		format.EncodeFixheader(&re, h)

		h2, err := format.DecodeFixheader(re)
		require.NoError(t, err, "re-encoded fixheader must decode")
		require.Equal(t, *h, *h2, "fixheader did not survive a decode/encode round trip")
	})
}
