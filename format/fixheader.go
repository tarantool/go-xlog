package format

import (
	"errors"
	"fmt"
	"math"
)

// Fixheader is the 19-byte preface to every tx block. The wire shape
// (src/box/xlog.c:1095-1105) is:
//
//	bytes 0-3    : Magic (4-byte raw)
//	mp_uint      : Len    — payload byte count after the fixheader
//	mp_uint      : CRC32P — always 0 (legacy, not validated on read)
//	mp_uint      : CRC32C — checksum over the on-disk payload bytes
//	mp_str(N)    : padding — N=(remaining-bytes-1) zero bytes, sized so the
//	               whole fixheader is exactly FixheaderSize
type Fixheader struct {
	Magic  [4]byte
	Len    uint32
	CRC32P uint32
	CRC32C uint32
}

// ErrShortFixheader / ErrUnknownMagic / ErrFixheaderShape — surfaced when
// DecodeFixheader rejects a malformed block.
var (
	ErrShortFixheader  = errors.New("format: fixheader: truncated")
	ErrUnknownMagic    = errors.New("format: fixheader: unknown magic")
	ErrFixheaderShape  = errors.New("format: fixheader: malformed shape")
	ErrPaddingTooLarge = errors.New("format: fixheader: padding exceeds fixheader size")
)

// EncodeFixheader writes a complete 19-byte fixheader into buf.
//
// The three uints are msgpack-encoded with the smallest width that fits
// (matching `mp_encode_uint` in mp.h). The remaining tail bytes are filled
// as an mp_str header with N-1 zero bytes of content, where N is the
// remaining byte count — exactly the recipe in src/box/xlog.c:1100-1105.
//
// EncodeFixheader panics if the encoded uints overflow 15 bytes, which
// cannot happen for uint32 fields (max 5+5+5 = 15 bytes after the 4-byte
// magic, leaving exactly 0 padding bytes — and `mp_encode_strl(0)` for a
// zero-length fixstr is one byte 0xa0, but since `padding == 0` the
// writer skips the str entirely, matching Tarantool's `if (padding > 0)`).
func EncodeFixheader(buf *[FixheaderSize]byte, h *Fixheader) {
	if h == nil {
		panic("format: EncodeFixheader: nil header")
	}
	// Start with magic.
	out := buf[:0]
	out = append(out, h.Magic[:]...)
	out = appendMPUint(out, uint64(h.Len))
	out = appendMPUint(out, uint64(h.CRC32P))
	out = appendMPUint(out, uint64(h.CRC32C))
	// Padding to reach FixheaderSize.
	padding := FixheaderSize - len(out)
	if padding < 0 {
		// Uint32 fields can produce at most 5 bytes each → 4+5+5+5=19 max.
		// So padding can be 0 in the worst case, never negative. Treat
		// this as a programmer error.
		panic(fmt.Sprintf("format: EncodeFixheader: overshot by %d bytes", -padding))
	}

	if padding > 0 {
		// Mp_str header for (padding-1) zero bytes — the strl byte itself
		// is one of the padding bytes, so we have padding-1 zeros after it.
		out = appendMPStrHeader(out, padding-1)
		// Now write (padding-1) zero bytes.
		for range padding - 1 {
			out = append(out, 0)
		}
	}
	// The buf is fixed-size; sanity check.
	if len(out) != FixheaderSize {
		panic(fmt.Sprintf("format: EncodeFixheader: produced %d bytes, want %d", len(out), FixheaderSize))
	}
}

// DecodeFixheader parses the 19 bytes and returns the Fixheader. It
// validates that the magic is one of {RowMarker, ZRowMarker, EOFMarker}
// and that the three uints and the trailing padding consume exactly
// FixheaderSize bytes. The padding bytes themselves are not inspected —
// Tarantool's writer always zeroes them but readers tolerate any content
// since it is mp_str-typed data (strict on shape, but the content is not inspected).
func DecodeFixheader(b [FixheaderSize]byte) (*Fixheader, error) {
	h := &Fixheader{}
	if err := DecodeFixheaderInto(b, h); err != nil {
		return nil, err
	}

	return h, nil
}

// DecodeFixheaderInto parses the 19 bytes into h, fully overwriting it. It is
// the zero-allocation core of DecodeFixheader: a reader driving a tight block
// loop reuses a single Fixheader instead of allocating one per tx block.
func DecodeFixheaderInto(b [FixheaderSize]byte, h *Fixheader) error {
	*h = Fixheader{}
	copy(h.Magic[:], b[0:4])

	if h.Magic != RowMarker && h.Magic != ZRowMarker && h.Magic != EOFMarker {
		return fmt.Errorf("%w: %x", ErrUnknownMagic, h.Magic[:])
	}

	off := 4

	u, n, err := readMPUint(b[off:])
	if err != nil {
		return fmt.Errorf("%w: len: %w", ErrFixheaderShape, err)
	}

	if u > math.MaxUint32 {
		return fmt.Errorf("%w: len exceeds uint32", ErrFixheaderShape)
	}

	h.Len = uint32(u)
	off += n

	u, n, err = readMPUint(b[off:])
	if err != nil {
		return fmt.Errorf("%w: crc32p: %w", ErrFixheaderShape, err)
	}

	h.CRC32P = uint32(u) //nolint:gosec // G115: crc32p is a uint32 fixheader field
	off += n

	u, n, err = readMPUint(b[off:])
	if err != nil {
		return fmt.Errorf("%w: crc32c: %w", ErrFixheaderShape, err)
	}

	h.CRC32C = uint32(u) //nolint:gosec // G115: crc32c is a uint32 fixheader field
	off += n

	if off > FixheaderSize {
		return fmt.Errorf("%w: uints overflowed fixheader", ErrFixheaderShape)
	}

	if off < FixheaderSize {
		// Padding mp_str must consume exactly the remaining bytes.
		consumed, err := skipMP(b[off:])
		if err != nil {
			return fmt.Errorf("%w: padding: %w", ErrFixheaderShape, err)
		}

		if off+consumed != FixheaderSize {
			return fmt.Errorf("%w: padding consumed %d, want %d", ErrFixheaderShape, consumed, FixheaderSize-off)
		}
	}

	return nil
}
