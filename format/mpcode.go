package format

// msgpack type-tag bytes — the first byte of every msgpack-encoded value, used
// by the hand-rolled append/peek helpers in mp.go (and the RAFT bool decode in
// body_raft.go). These are the canonical msgpack spec codes; they were
// previously pulled from github.com/vmihailenco/msgpack/v5/msgpcode, but the
// library needs that dependency only for these constants, so they are inlined
// here to keep package format (and everything that imports it) free of any
// third-party msgpack code. Values match the spec exactly:
// https://github.com/msgpack/msgpack/blob/master/spec.md.
const (
	mpcPosFixedNumHigh byte = 0x7f
	mpcNegFixedNumLow  byte = 0xe0

	mpcNil byte = 0xc0

	mpcFalse byte = 0xc2
	mpcTrue  byte = 0xc3

	mpcFloat  byte = 0xca
	mpcDouble byte = 0xcb

	mpcUint8  byte = 0xcc
	mpcUint16 byte = 0xcd
	mpcUint32 byte = 0xce
	mpcUint64 byte = 0xcf

	mpcInt8  byte = 0xd0
	mpcInt16 byte = 0xd1
	mpcInt32 byte = 0xd2
	mpcInt64 byte = 0xd3

	mpcFixedStrLow  byte = 0xa0
	mpcFixedStrHigh byte = 0xbf
	mpcFixedStrMask byte = 0x1f
	mpcStr8         byte = 0xd9
	mpcStr16        byte = 0xda
	mpcStr32        byte = 0xdb

	mpcBin8  byte = 0xc4
	mpcBin16 byte = 0xc5
	mpcBin32 byte = 0xc6

	mpcFixedArrayLow  byte = 0x90
	mpcFixedArrayHigh byte = 0x9f
	mpcFixedArrayMask byte = 0x0f
	mpcArray16        byte = 0xdc
	mpcArray32        byte = 0xdd

	mpcFixedMapLow  byte = 0x80
	mpcFixedMapHigh byte = 0x8f
	mpcFixedMapMask byte = 0x0f
	mpcMap16        byte = 0xde
	mpcMap32        byte = 0xdf

	mpcFixExt1  byte = 0xd4
	mpcFixExt2  byte = 0xd5
	mpcFixExt4  byte = 0xd6
	mpcFixExt8  byte = 0xd7
	mpcFixExt16 byte = 0xd8
	mpcExt8     byte = 0xc7
	mpcExt16    byte = 0xc8
	mpcExt32    byte = 0xc9
)
