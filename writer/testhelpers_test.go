package writer //nolint:testpackage // defines internal-only test helpers (encodeDMLBody/fixedDMLBody) shared with white-box tests

import (
	"encoding/binary"

	"github.com/vmihailenco/msgpack/v5/msgpcode"
)

// encodeDMLBody builds a DML body map {KeySpaceID: 512, KeyTuple: [tupleVals...]}
// as msgpack bytes. Returns bytes suitable to assign to XRow.BodyRaw.
//
// Lives in the test package because format/ does not currently expose an
// EncodeDMLBody helper (only typed-body decoders ship today).
func encodeDMLBody(tupleVals []uint64) []byte {
	const spaceID = 512

	var b []byte

	b = appendMPMapHeader(b, 2)
	// KeySpaceID (0x10) — fits in fixint.
	b = appendMPUint(b, 0x10)
	b = appendMPUint(b, spaceID)
	// KeyTuple (0x21).
	b = appendMPUint(b, 0x21)

	b = appendMPArrayHeader(b, len(tupleVals))
	for _, v := range tupleVals {
		b = appendMPUint(b, v)
	}

	return b
}

// fixedDMLBody returns a DML body where the KeyTuple value is a single
// msgpack bin of `padLen` bytes — useful for producing payloads that cross
// the compression threshold without depending on stable int encoding sizes.
func fixedDMLBody(spaceID uint32, padLen int) []byte {
	var b []byte

	b = appendMPMapHeader(b, 2)
	b = appendMPUint(b, 0x10)
	b = appendMPUint(b, uint64(spaceID))
	b = appendMPUint(b, 0x21)
	// Tuple is an array of one bin.
	b = appendMPArrayHeader(b, 1)
	b = appendMPBin(b, make([]byte, padLen))

	return b
}

// --- minimal msgpack append helpers (test-only, mirrors format/mp.go) ---.

func appendMPUint(buf []byte, n uint64) []byte {
	switch {
	case n <= 0x7f:
		return append(buf, byte(n))
	case n <= 0xff:
		return append(buf, msgpcode.Uint8, byte(n))
	case n <= 0xffff:
		buf = append(buf, msgpcode.Uint16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	case n <= 0xffffffff:
		buf = append(buf, msgpcode.Uint32)

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(n))

		return append(buf, tmp[:]...)
	default:
		buf = append(buf, msgpcode.Uint64)

		var tmp [8]byte
		binary.BigEndian.PutUint64(tmp[:], n)

		return append(buf, tmp[:]...)
	}
}

func appendMPMapHeader(buf []byte, n int) []byte {
	if n <= 15 {
		return append(buf, msgpcode.FixedMapLow|byte(n))
	}

	if n <= 0xffff {
		buf = append(buf, msgpcode.Map16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	}

	buf = append(buf, msgpcode.Map32)

	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(n))

	return append(buf, tmp[:]...)
}

func appendMPArrayHeader(buf []byte, n int) []byte {
	if n <= 15 {
		return append(buf, msgpcode.FixedArrayLow|byte(n))
	}

	if n <= 0xffff {
		buf = append(buf, msgpcode.Array16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	}

	buf = append(buf, msgpcode.Array32)

	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(n))

	return append(buf, tmp[:]...)
}

func appendMPBin(buf []byte, p []byte) []byte {
	n := len(p)
	switch {
	case n <= 0xff:
		buf = append(buf, msgpcode.Bin8, byte(n))
	case n <= 0xffff:
		buf = append(buf, msgpcode.Bin16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))
		buf = append(buf, tmp[:]...)
	default:
		buf = append(buf, msgpcode.Bin32)

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(n))
		buf = append(buf, tmp[:]...)
	}

	return append(buf, p...)
}
