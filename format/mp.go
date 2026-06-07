package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// This file holds small msgpack append/peek helpers. A general msgpack library
// is Encoder/Decoder-shaped (io.Reader / io.Writer); for byte-exact encoding of
// the few primitives used in the on-disk format (uints, mp_str padding, map
// prefixes) we need direct []byte append helpers. The type-tag byte constants
// (mpc*) live in mpcode.go so the package carries no third-party msgpack dep.

// Sentinel errors for the msgpack peek/skip helpers. Each call site wraps
// one of these with its specific prefix so the rendered message stays
// byte-for-byte identical to the historical hand-written strings while
// satisfying err113 (static, comparable error values).
var (
	ErrEmptyInput      = errors.New("empty input")
	ErrShortInput      = errors.New("short input")
	ErrTruncatedInput  = errors.New("truncated input")
	ErrUnexpectedTag   = errors.New("unexpected type tag")
	ErrUnsupportedTag  = errors.New("unsupported type tag")
	ErrShortHeader     = errors.New("header")
	ErrShortPayload    = errors.New("payload")
	ErrShortFixedWidth = errors.New("short fixed-width value")
	ErrShortFixstr     = errors.New("short fixstr")
)

// msgpack header widths: a one-byte type tag followed by a big-endian length
// or value field. The str/bin/array/map "16"/"32" variants share these — a
// str16 header and a uint16 value are both a tag plus two bytes.
const (
	mpTagSize = 1 // Every msgpack value begins with a one-byte type tag.

	mpHead8  = mpTagSize + 1 // Tag + 8-bit  field (uint8, str8, bin8, ext payload byte).
	mpHead16 = mpTagSize + 2 // Tag + 16-bit field (uint16, str16, array16, map16, ...)
	mpHead32 = mpTagSize + 4 // Tag + 32-bit field (uint32, str32, array32, map32, ...)
	mpHead64 = mpTagSize + 8 // Tag + 64-bit field (uint64, int64, double).

	// Ext headers carry a one-byte ext-type after the length field.
	mpExtTypeSize = 1
	mpHeadExt8    = mpHead8 + mpExtTypeSize  // 3
	mpHeadExt16   = mpHead16 + mpExtTypeSize // 4
	mpHeadExt32   = mpHead32 + mpExtTypeSize // 6

	// Fixext values are a tag + one-byte ext-type + a fixed data width.
	mpFixExtHeader = mpTagSize + mpExtTypeSize // 2
	mpFixExt1Size  = mpFixExtHeader + 1        // 3
	mpFixExt2Size  = mpFixExtHeader + 2        // 4
	mpFixExt4Size  = mpFixExtHeader + 4        // 6
	mpFixExt8Size  = mpFixExtHeader + 8        // 10
	mpFixExt16Size = mpFixExtHeader + 16       // 18

	// MpMapEntryFields is the number of msgpack values per map entry: a key
	// plus a value. A map of N entries therefore contains 2N child values.
	mpMapEntryFields = 2
)

// appendMPUint appends a msgpack-encoded uint to buf using the smallest
// width that fits, matching `mp_encode_uint` in mp.h.
func appendMPUint(buf []byte, n uint64) []byte {
	switch {
	case n <= uint64(mpcPosFixedNumHigh): // Positive fixint.
		return append(buf, byte(n)) // n<=0x7f (positive fixint) in this case.
	case n <= math.MaxUint8:
		return append(buf, mpcUint8, byte(n))
	case n <= math.MaxUint16:
		buf = append(buf, mpcUint16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	case n <= math.MaxUint32:
		buf = append(buf, mpcUint32)

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(n))

		return append(buf, tmp[:]...)
	default:
		buf = append(buf, mpcUint64)

		var tmp [8]byte
		binary.BigEndian.PutUint64(tmp[:], n)

		return append(buf, tmp[:]...)
	}
}

// appendMPDouble appends a msgpack-encoded double (mpcDouble, 0xcb)
// matching mp_encode_double. Used for IPROTO_TIMESTAMP.
func appendMPDouble(buf []byte, f float64) []byte {
	buf = append(buf, mpcDouble)

	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], math.Float64bits(f))

	return append(buf, tmp[:]...)
}

// appendMPStrHeader appends an mp_str type tag + length, then leaves the
// caller to write the n payload bytes themselves. Matches mp_encode_strl.
func appendMPStrHeader(buf []byte, n int) []byte {
	switch {
	case n <= int(mpcFixedStrMask): // fixstr.
		return append(buf, mpcFixedStrLow|byte(n)) //nolint:gosec // G115: n<=31 in this case
	case n <= math.MaxUint8:
		return append(buf, mpcStr8, byte(n)) // n<=math.MaxUint8 in this case.
	case n <= math.MaxUint16:
		buf = append(buf, mpcStr16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	default:
		buf = append(buf, mpcStr32)

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(n)) //nolint:gosec // G115: str length bounded by math.MaxUint32 in this case branch

		return append(buf, tmp[:]...)
	}
}

// appendMPMapHeader appends an mp_map type tag + length. Used for the xrow header-map prefix
// when we know the entry count up front.
func appendMPMapHeader(buf []byte, n int) []byte {
	switch {
	case n <= int(mpcFixedMapMask):
		return append(buf, mpcFixedMapLow|byte(n)) //nolint:gosec // G115: n<=15 in this case
	case n <= math.MaxUint16:
		buf = append(buf, mpcMap16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n)) // n<=math.MaxUint16 in this case.

		return append(buf, tmp[:]...)
	default:
		buf = append(buf, mpcMap32)

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(n)) //nolint:gosec // G115: map length bounded by math.MaxUint32 in this case branch

		return append(buf, tmp[:]...)
	}
}

// readMPUint reads a msgpack uint at b[0:] and returns (value, bytes-read).
// It accepts positive fixint, uint8, uint16, uint32, uint64.
func readMPUint(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("format: mp uint: %w", ErrEmptyInput)
	}

	c := b[0]
	if c <= mpcPosFixedNumHigh {
		return uint64(c), 1, nil
	}

	switch c {
	case mpcUint8:
		if len(b) < mpHead8 {
			return 0, 0, fmt.Errorf("format: mp uint8: %w", ErrShortInput)
		}

		return uint64(b[1]), mpHead8, nil
	case mpcUint16:
		if len(b) < mpHead16 {
			return 0, 0, fmt.Errorf("format: mp uint16: %w", ErrShortInput)
		}

		return uint64(binary.BigEndian.Uint16(b[1:mpHead16])), mpHead16, nil
	case mpcUint32:
		if len(b) < mpHead32 {
			return 0, 0, fmt.Errorf("format: mp uint32: %w", ErrShortInput)
		}

		return uint64(binary.BigEndian.Uint32(b[1:mpHead32])), mpHead32, nil
	case mpcUint64:
		if len(b) < mpHead64 {
			return 0, 0, fmt.Errorf("format: mp uint64: %w", ErrShortInput)
		}

		return binary.BigEndian.Uint64(b[1:mpHead64]), mpHead64, nil
	default:
		return 0, 0, fmt.Errorf("format: mp uint: %w 0x%02x", ErrUnexpectedTag, c)
	}
}

// readMPMapLen reads a msgpack map header and returns (entries, header-len).
func readMPMapLen(b []byte) (int, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("format: mp map: %w", ErrEmptyInput)
	}

	c := b[0]
	switch {
	case c >= mpcFixedMapLow && c <= mpcFixedMapHigh:
		return int(c & mpcFixedMapMask), 1, nil
	case c == mpcMap16:
		if len(b) < mpHead16 {
			return 0, 0, fmt.Errorf("format: mp map16: %w", ErrShortInput)
		}

		return int(binary.BigEndian.Uint16(b[1:mpHead16])), mpHead16, nil
	case c == mpcMap32:
		if len(b) < mpHead32 {
			return 0, 0, fmt.Errorf("format: mp map32: %w", ErrShortInput)
		}

		return int(binary.BigEndian.Uint32(b[1:mpHead32])), mpHead32, nil
	default:
		return 0, 0, fmt.Errorf("format: mp map: %w 0x%02x", ErrUnexpectedTag, c)
	}
}

// readMPDouble reads a msgpack float32 / float64 and returns (value, n-read).
// Tarantool always encodes IPROTO_TIMESTAMP as float64 (mpcDouble).
func readMPDouble(b []byte) (float64, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("format: mp double: %w", ErrEmptyInput)
	}

	switch b[0] {
	case mpcFloat:
		if len(b) < mpHead32 {
			return 0, 0, fmt.Errorf("format: mp float: %w", ErrShortInput)
		}

		u := binary.BigEndian.Uint32(b[1:mpHead32])

		return float64(math.Float32frombits(u)), mpHead32, nil
	case mpcDouble:
		if len(b) < mpHead64 {
			return 0, 0, fmt.Errorf("format: mp double: %w", ErrShortInput)
		}

		u := binary.BigEndian.Uint64(b[1:mpHead64])

		return math.Float64frombits(u), mpHead64, nil
	default:
		return 0, 0, fmt.Errorf("format: mp double: %w 0x%02x", ErrUnexpectedTag, b[0])
	}
}

// skipMP advances past one msgpack value at b[0:] and returns the byte
// count consumed. Used by xrow header parsing to skip unknown keys / values
// and to splice off the body region.
//
// It supports every msgpack type the on-disk format may carry — uints,
// ints, floats, bools, nil, strings, bins, arrays, maps, exts. The walk is
// iterative via a small stack so deeply nested arrays/maps don't blow Go's
// call stack on hostile input.
func skipMP(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("format: skipMP: %w", ErrEmptyInput)
	}
	// Iterative walker — track how many more "values" we owe at the current
	// container nesting level via a stack. Back it with a fixed array so the
	// common (shallow) case stays on the goroutine stack; append only spills
	// to the heap for msgpack nested past stackDepth levels, which xlog bodies
	// never are. This keeps body-skipping (and thus DecodeXRow) alloc-free.
	const stackDepth = 32

	var stackArr [stackDepth]int

	stack := stackArr[:1]
	stack[0] = 1

	off := 0
	for len(stack) > 0 {
		if off >= len(b) {
			return 0, fmt.Errorf("format: skipMP: %w", ErrTruncatedInput)
		}
		// Consume one value at off.
		n, children, err := mpHead(b[off:])
		if err != nil {
			return 0, err
		}

		off += n
		// We just consumed one value at this nesting level.
		stack[len(stack)-1]--
		if children > 0 {
			stack = append(stack, children)
		}
		// Pop fully-consumed levels.
		for len(stack) > 0 && stack[len(stack)-1] == 0 {
			stack = stack[:len(stack)-1]
		}
	}

	return off, nil
}

// mpHead reads the type tag + size header of one msgpack value at b[0:],
// consuming any inline payload (numbers, strings, bins, exts). For arrays
// and maps it returns the number of *child values* still to consume (twice
// the entry count for maps). Returns (header+payload bytes, children, err).
func mpHead(b []byte) (int, int, error) {
	c := b[0]
	// Fixed reports a value of exactly sz bytes, guarding that the buffer
	// actually holds them — a truncated fixed-width value must error, never
	// claim bytes past the end (which would let skipMP run off the buffer).
	fixed := func(sz int) (int, int, error) {
		if sz > len(b) {
			return 0, 0, fmt.Errorf("format: mpHead: %w", ErrShortFixedWidth)
		}

		return sz, 0, nil
	}

	switch {
	case c <= mpcPosFixedNumHigh, c >= mpcNegFixedNumLow: // Positive fixint / negative fixint.
		return mpTagSize, 0, nil
	case c >= mpcFixedStrLow && c <= mpcFixedStrHigh:
		l := int(c & mpcFixedStrMask)
		if mpTagSize+l > len(b) {
			return 0, 0, fmt.Errorf("format: mpHead: %w", ErrShortFixstr)
		}

		return mpTagSize + l, 0, nil
	case c >= mpcFixedArrayLow && c <= mpcFixedArrayHigh:
		return mpTagSize, int(c & mpcFixedArrayMask), nil
	case c >= mpcFixedMapLow && c <= mpcFixedMapHigh:
		return mpTagSize, mpMapEntryFields * int(c&mpcFixedMapMask), nil
	}

	switch c {
	case mpcNil, mpcFalse, mpcTrue:
		return mpTagSize, 0, nil
	case mpcUint8, mpcInt8:
		return fixed(mpHead8)
	case mpcUint16, mpcInt16:
		return fixed(mpHead16)
	case mpcUint32, mpcInt32, mpcFloat:
		return fixed(mpHead32)
	case mpcUint64, mpcInt64, mpcDouble:
		return fixed(mpHead64)
	case mpcStr8, mpcBin8:
		if len(b) < mpHead8 {
			return 0, 0, fmt.Errorf("format: mpHead: short str8/bin8 %w", ErrShortHeader)
		}

		l := int(b[1])
		if mpHead8+l > len(b) {
			return 0, 0, fmt.Errorf("format: mpHead: short str8/bin8 %w", ErrShortPayload)
		}

		return mpHead8 + l, 0, nil
	case mpcStr16, mpcBin16:
		if len(b) < mpHead16 {
			return 0, 0, fmt.Errorf("format: mpHead: short str16/bin16 %w", ErrShortHeader)
		}

		l := int(binary.BigEndian.Uint16(b[1:mpHead16]))
		if mpHead16+l > len(b) {
			return 0, 0, fmt.Errorf("format: mpHead: short str16/bin16 %w", ErrShortPayload)
		}

		return mpHead16 + l, 0, nil
	case mpcStr32, mpcBin32:
		if len(b) < mpHead32 {
			return 0, 0, fmt.Errorf("format: mpHead: short str32/bin32 %w", ErrShortHeader)
		}

		l := int(binary.BigEndian.Uint32(b[1:mpHead32]))
		if mpHead32+l > len(b) {
			return 0, 0, fmt.Errorf("format: mpHead: short str32/bin32 %w", ErrShortPayload)
		}

		return mpHead32 + l, 0, nil
	case mpcArray16:
		if len(b) < mpHead16 {
			return 0, 0, fmt.Errorf("format: mpHead: short array16 %w", ErrShortHeader)
		}

		return mpHead16, int(binary.BigEndian.Uint16(b[1:mpHead16])), nil
	case mpcArray32:
		if len(b) < mpHead32 {
			return 0, 0, fmt.Errorf("format: mpHead: short array32 %w", ErrShortHeader)
		}

		return mpHead32, int(binary.BigEndian.Uint32(b[1:mpHead32])), nil
	case mpcMap16:
		if len(b) < mpHead16 {
			return 0, 0, fmt.Errorf("format: mpHead: short map16 %w", ErrShortHeader)
		}

		return mpHead16, mpMapEntryFields * int(binary.BigEndian.Uint16(b[1:mpHead16])), nil
	case mpcMap32:
		if len(b) < mpHead32 {
			return 0, 0, fmt.Errorf("format: mpHead: short map32 %w", ErrShortHeader)
		}

		return mpHead32, mpMapEntryFields * int(binary.BigEndian.Uint32(b[1:mpHead32])), nil
	case mpcFixExt1:
		return fixed(mpFixExt1Size)
	case mpcFixExt2:
		return fixed(mpFixExt2Size)
	case mpcFixExt4:
		return fixed(mpFixExt4Size)
	case mpcFixExt8:
		return fixed(mpFixExt8Size)
	case mpcFixExt16:
		return fixed(mpFixExt16Size)
	case mpcExt8:
		if len(b) < mpHeadExt8 {
			return 0, 0, fmt.Errorf("format: mpHead: short ext8 %w", ErrShortHeader)
		}

		l := int(b[1])
		if mpHeadExt8+l > len(b) {
			return 0, 0, fmt.Errorf("format: mpHead: short ext8 %w", ErrShortPayload)
		}

		return mpHeadExt8 + l, 0, nil
	case mpcExt16:
		if len(b) < mpHeadExt16 {
			return 0, 0, fmt.Errorf("format: mpHead: short ext16 %w", ErrShortHeader)
		}

		l := int(binary.BigEndian.Uint16(b[1:mpHead16]))
		if mpHeadExt16+l > len(b) {
			return 0, 0, fmt.Errorf("format: mpHead: short ext16 %w", ErrShortPayload)
		}

		return mpHeadExt16 + l, 0, nil
	case mpcExt32:
		if len(b) < mpHeadExt32 {
			return 0, 0, fmt.Errorf("format: mpHead: short ext32 %w", ErrShortHeader)
		}

		l := int(binary.BigEndian.Uint32(b[1:mpHead32]))
		if mpHeadExt32+l > len(b) {
			return 0, 0, fmt.Errorf("format: mpHead: short ext32 %w", ErrShortPayload)
		}

		return mpHeadExt32 + l, 0, nil
	}

	return 0, 0, fmt.Errorf("format: mpHead: %w 0x%02x", ErrUnsupportedTag, c)
}
