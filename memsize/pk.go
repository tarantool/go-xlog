package memsize

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/tarantool/go-xlog/format"
)

const (
	mpNil      = byte(0xc0)
	mpFalse    = byte(0xc2)
	mpTrue     = byte(0xc3)
	mpBin8     = byte(0xc4)
	mpBin16    = byte(0xc5)
	mpBin32    = byte(0xc6)
	mpExt8     = byte(0xc7)
	mpExt16    = byte(0xc8)
	mpExt32    = byte(0xc9)
	mpFloat32  = byte(0xca)
	mpFloat64  = byte(0xcb)
	mpUint8    = byte(0xcc)
	mpUint16   = byte(0xcd)
	mpUint32   = byte(0xce)
	mpUint64   = byte(0xcf)
	mpInt8     = byte(0xd0)
	mpInt16    = byte(0xd1)
	mpInt32    = byte(0xd2)
	mpInt64    = byte(0xd3)
	mpFixExt1  = byte(0xd4)
	mpFixExt2  = byte(0xd5)
	mpFixExt4  = byte(0xd6)
	mpFixExt8  = byte(0xd7)
	mpFixExt16 = byte(0xd8)
	mpStr8     = byte(0xd9)
	mpStr16    = byte(0xda)
	mpStr32    = byte(0xdb)
	mpArray16  = byte(0xdc)
	mpArray32  = byte(0xdd)
	mpMap16    = byte(0xde)
	mpMap32    = byte(0xdf)

	mpPositiveFixintMax = byte(0x7f)
	mpFixMapBase        = byte(0x80)
	mpFixArrayBase      = byte(0x90)
	mpFixStrBase        = byte(0xa0)
	mpNegativeFixintMin = byte(0xe0)

	mpFixMapMax   = 15
	mpFixArrayMax = 15
	mpFixStrMax   = 31
	mpFloat32Size = 5
	mpFloat64Size = 9

	mpLength8HeaderSize  = 2
	mpLength16HeaderSize = 3
	mpLength32HeaderSize = 5
	mpFixExtHeaderSize   = 2

	mpFixExt1PayloadSize  = 1
	mpFixExt2PayloadSize  = 2
	mpFixExt4PayloadSize  = 4
	mpFixExt8PayloadSize  = 8
	mpFixExt16PayloadSize = 16

	maxUint64Exclusive = 1 << 64

	fnv64Offset = uint64(14695981039346656037)
	fnv64Prime  = uint64(1099511628211)
)

// Primary-key errors are comparable sentinels for errors.Is.
var (
	ErrInvalidPrimaryKey       = errors.New("invalid primary key")
	ErrPrimaryKeyFieldMissing  = errors.New("primary-key field is missing")
	ErrUnsupportedPKCollation  = errors.New("primary-key collation is unsupported offline")
	ErrUnsupportedPKType       = errors.New("primary-key type is unsupported offline")
	ErrUnsupportedPrimaryPath  = errors.New("primary-key JSON path is unsupported offline")
	ErrUnsupportedPrimaryValue = errors.New("primary-key value is unsupported offline")
)

type primaryKeyScratch struct {
	values  [][]byte
	encoded []byte
}

// PrimaryKey returns the canonical key's 64-bit FNV-1a hash. Replay stores
// hashes rather than full keys so checker memory is proportional to churn;
// equal Tarantool numeric values hash equally even when their MessagePack
// widths differ.
func PrimaryKey(tuple []byte, pk *Index) (uint64, error) {
	var scratch primaryKeyScratch

	return scratch.tupleHash(tuple, pk)
}

// PrimaryKeyBytes returns a minimally encoded MessagePack array containing
// the tuple's primary-key parts in index order. Returned storage is owned by
// the caller.
func PrimaryKeyBytes(tuple []byte, pk *Index) ([]byte, error) {
	if err := validatePrimaryKey(pk); err != nil {
		return nil, err
	}

	values, err := primaryKeyTupleValues(tuple, pk, make([][]byte, len(pk.Parts)))
	if err != nil {
		return nil, err
	}

	return appendCanonicalPrimaryKey(nil, values, pk)
}

func (s *primaryKeyScratch) tupleHash(tuple []byte, pk *Index) (uint64, error) {
	if err := validatePrimaryKey(pk); err != nil {
		return 0, err
	}

	values, err := primaryKeyTupleValues(tuple, pk, s.prepareValues(len(pk.Parts)))
	if err != nil {
		return 0, err
	}

	return s.hash(values, pk)
}

func (s *primaryKeyScratch) keyHash(key []byte, pk *Index) (uint64, error) {
	if err := validatePrimaryKey(pk); err != nil {
		return 0, err
	}

	values, err := primaryKeyKeyValues(key, pk, s.prepareValues(len(pk.Parts)))
	if err != nil {
		return 0, err
	}

	return s.hash(values, pk)
}

func (s *primaryKeyScratch) prepareValues(count int) [][]byte {
	if cap(s.values) < count {
		s.values = make([][]byte, count)
	} else {
		s.values = s.values[:count]
		clear(s.values)
	}

	return s.values
}

func (s *primaryKeyScratch) hash(values [][]byte, pk *Index) (uint64, error) {
	var err error

	s.encoded, err = appendCanonicalPrimaryKey(s.encoded[:0], values, pk)
	if err != nil {
		return 0, err
	}

	hash := fnv64Offset
	for _, value := range s.encoded {
		hash ^= uint64(value)
		hash *= fnv64Prime
	}

	return hash, nil
}

func primaryKeyTupleValues(tuple []byte, pk *Index, values [][]byte) ([][]byte, error) {
	cursor := format.NewMPCursor(tuple)

	fieldCount, err := cursor.ArrayLen()
	if err != nil {
		return nil, fmt.Errorf("memsize: primary key: tuple: %w: %w", ErrInvalidPrimaryKey, err)
	}

	remaining := len(values)

	for fieldNo := 0; fieldNo < fieldCount && remaining > 0; fieldNo++ {
		raw, err := cursor.Raw()
		if err != nil {
			return nil, fmt.Errorf("memsize: primary key: tuple field %d: %w: %w",
				fieldNo, ErrInvalidPrimaryKey, err)
		}

		for partNo := range pk.Parts {
			if values[partNo] == nil && pk.Parts[partNo].FieldNo == uint32(fieldNo) {
				values[partNo] = raw
				remaining--
			}
		}
	}

	if remaining != 0 {
		return nil, fmt.Errorf("memsize: primary key: %w: tuple has %d fields", ErrPrimaryKeyFieldMissing, fieldCount)
	}

	return values, nil
}

func primaryKeyKeyValues(key []byte, pk *Index, values [][]byte) ([][]byte, error) {
	cursor := format.NewMPCursor(key)

	partCount, err := cursor.ArrayLen()
	if err != nil {
		return nil, fmt.Errorf("memsize: primary key: key tuple: %w: %w", ErrInvalidPrimaryKey, err)
	}

	if partCount != len(pk.Parts) {
		return nil, fmt.Errorf("memsize: primary key: %w: got %d key parts, need %d",
			ErrInvalidPrimaryKey, partCount, len(pk.Parts))
	}

	for partNo := range partCount {
		values[partNo], err = cursor.Raw()
		if err != nil {
			return nil, fmt.Errorf("memsize: primary key: key part %d: %w: %w",
				partNo, ErrInvalidPrimaryKey, err)
		}
	}

	return values, nil
}

func validatePrimaryKey(pk *Index) error {
	if pk == nil || len(pk.Parts) == 0 {
		return fmt.Errorf("memsize: primary key: %w: missing primary index parts", ErrInvalidPrimaryKey)
	}

	return validatePrimaryKeyParts(pk)
}

func validatePrimaryKeyParts(pk *Index) error {
	for partNo := range pk.Parts {
		part := &pk.Parts[partNo]
		if part.Collation != "" {
			return fmt.Errorf("memsize: primary key part %d collation %q: %w",
				partNo, part.Collation, ErrUnsupportedPKCollation)
		}

		if strings.EqualFold(part.Type, "decimal") {
			return fmt.Errorf("memsize: primary key part %d type %q: %w",
				partNo, part.Type, ErrUnsupportedPKType)
		}

		if part.path != "" {
			return fmt.Errorf("memsize: primary key part %d path %q: %w",
				partNo, part.path, ErrUnsupportedPrimaryPath)
		}
	}

	return nil
}

func appendCanonicalPrimaryKey(dst []byte, values [][]byte, pk *Index) ([]byte, error) {
	key := appendMPArrayHeader(dst, len(values))
	for partNo, raw := range values {
		var err error

		key, err = appendCanonicalPart(key, raw, &pk.Parts[partNo])
		if err != nil {
			return nil, fmt.Errorf("memsize: primary key part %d: %w", partNo, err)
		}
	}

	return key, nil
}

func appendCanonicalPart(dst, raw []byte, part *Part) ([]byte, error) {
	switch strings.ToLower(part.Type) {
	case "unsigned", "integer", "number", "double":
		return appendCanonicalNumber(dst, raw)
	case "string":
		cursor := format.NewMPCursor(raw)

		value, err := cursor.Str()
		if err != nil {
			return nil, fmt.Errorf("%w: string: %w", ErrInvalidPrimaryKey, err)
		}

		return appendMPString(dst, value), nil
	default:
		return appendCanonicalValue(dst, raw)
	}
}

func appendCanonicalNumber(dst, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty number", ErrInvalidPrimaryKey)
	}

	switch raw[0] {
	case mpFloat32:
		if len(raw) != mpFloat32Size {
			return nil, fmt.Errorf("%w: truncated float32", ErrInvalidPrimaryKey)
		}

		value := float64(math.Float32frombits(binary.BigEndian.Uint32(raw[1:])))

		return appendCanonicalFloat(dst, value)
	case mpFloat64:
		if len(raw) != mpFloat64Size {
			return nil, fmt.Errorf("%w: truncated float64", ErrInvalidPrimaryKey)
		}

		value := math.Float64frombits(binary.BigEndian.Uint64(raw[1:]))

		return appendCanonicalFloat(dst, value)
	default:
		cursor := format.NewMPCursor(raw)
		if value, err := cursor.Int(); err == nil {
			return appendMPInt(dst, value), nil
		}

		cursor = format.NewMPCursor(raw)

		value, err := cursor.Uint()
		if err != nil {
			return nil, fmt.Errorf("%w: number: %w", ErrInvalidPrimaryKey, err)
		}

		return appendMPUint(dst, value), nil
	}
}

func appendCanonicalFloat(dst []byte, value float64) ([]byte, error) {
	if math.IsNaN(value) {
		return nil, fmt.Errorf("%w: NaN", ErrUnsupportedPrimaryValue)
	}

	if math.Trunc(value) == value {
		switch {
		case value >= 0 && value < maxUint64Exclusive:
			return appendMPUint(dst, uint64(value)), nil
		case value >= math.MinInt64 && value < 0:
			return appendMPInt(dst, int64(value)), nil
		}
	}

	dst = append(dst, mpFloat64)

	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], math.Float64bits(value))

	return append(dst, encoded[:]...), nil
}

func appendCanonicalValue(dst, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty value", ErrInvalidPrimaryKey)
	}

	tag := raw[0]
	switch {
	case tag <= mpPositiveFixintMax || tag >= mpNegativeFixintMin,
		tag >= mpUint8 && tag <= mpInt64,
		tag == mpFloat32 || tag == mpFloat64:
		return appendCanonicalNumber(dst, raw)
	case tag == mpNil || tag == mpFalse || tag == mpTrue:
		return append(dst, tag), nil
	case tag >= mpFixStrBase && tag < mpFixStrBase+32 || tag == mpStr8 || tag == mpStr16 || tag == mpStr32:
		cursor := format.NewMPCursor(raw)

		value, err := cursor.Str()
		if err != nil {
			return nil, fmt.Errorf("%w: string: %w", ErrInvalidPrimaryKey, err)
		}

		return appendMPString(dst, value), nil
	case tag == mpBin8 || tag == mpBin16 || tag == mpBin32:
		payload, err := mpPayload(raw, false)
		if err != nil {
			return nil, err
		}

		return appendMPBinary(dst, payload), nil
	case tag >= mpFixArrayBase && tag < mpFixArrayBase+16 || tag == mpArray16 || tag == mpArray32:
		cursor := format.NewMPCursor(raw)

		count, err := cursor.ArrayLen()
		if err != nil {
			return nil, fmt.Errorf("%w: array: %w", ErrInvalidPrimaryKey, err)
		}

		dst = appendMPArrayHeader(dst, count)
		for item := range count {
			value, err := cursor.Raw()
			if err != nil {
				return nil, fmt.Errorf("%w: array item %d: %w", ErrInvalidPrimaryKey, item, err)
			}

			dst, err = appendCanonicalValue(dst, value)
			if err != nil {
				return nil, err
			}
		}

		return dst, nil
	case tag >= mpFixMapBase && tag < mpFixMapBase+16 || tag == mpMap16 || tag == mpMap32:
		cursor := format.NewMPCursor(raw)

		count, err := cursor.MapLen()
		if err != nil {
			return nil, fmt.Errorf("%w: map: %w", ErrInvalidPrimaryKey, err)
		}

		dst = appendMPMapHeader(dst, count)
		for entry := range count {
			for valueNo := range 2 {
				value, err := cursor.Raw()
				if err != nil {
					return nil, fmt.Errorf("%w: map entry %d value %d: %w",
						ErrInvalidPrimaryKey, entry, valueNo, err)
				}

				dst, err = appendCanonicalValue(dst, value)
				if err != nil {
					return nil, err
				}
			}
		}

		return dst, nil
	case tag >= mpFixExt1 && tag <= mpFixExt16 || tag == mpExt8 || tag == mpExt16 || tag == mpExt32:
		return appendCanonicalExt(dst, raw)
	default:
		return nil, fmt.Errorf("%w: MessagePack tag 0x%02x", ErrUnsupportedPrimaryValue, tag)
	}
}

func appendMPUint(dst []byte, value uint64) []byte {
	switch {
	case value <= uint64(mpPositiveFixintMax):
		return append(dst, byte(value))
	case value <= math.MaxUint8:
		return append(dst, mpUint8, byte(value))
	case value <= math.MaxUint16:
		dst = append(dst, mpUint16)

		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(value))

		return append(dst, encoded[:]...)
	case value <= math.MaxUint32:
		dst = append(dst, mpUint32)

		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], uint32(value))

		return append(dst, encoded[:]...)
	default:
		dst = append(dst, mpUint64)

		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], value)

		return append(dst, encoded[:]...)
	}
}

func appendMPInt(dst []byte, value int64) []byte {
	if value >= 0 {
		return appendMPUint(dst, uint64(value))
	}

	switch {
	case value >= -32:
		return append(dst, byte(int8(value))) //nolint:gosec // MessagePack stores the range-checked two's-complement byte.
	case value >= math.MinInt8:
		return append(dst, mpInt8, byte(int8(value))) //nolint:gosec // MessagePack stores the range-checked two's-complement byte.
	case value >= math.MinInt16:
		dst = append(dst, mpInt16)

		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(int16(value))) //nolint:gosec // MessagePack stores range-checked two's-complement bits.

		return append(dst, encoded[:]...)
	case value >= math.MinInt32:
		dst = append(dst, mpInt32)

		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], uint32(int32(value))) //nolint:gosec // MessagePack stores range-checked two's-complement bits.

		return append(dst, encoded[:]...)
	default:
		dst = append(dst, mpInt64)

		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(value)) //nolint:gosec // MessagePack stores two's-complement bits.

		return append(dst, encoded[:]...)
	}
}

func appendMPString(dst, value []byte) []byte {
	dst = appendMPStringHeader(dst, len(value))

	return append(dst, value...)
}

func appendMPStringHeader(dst []byte, size int) []byte {
	switch {
	case size <= mpFixStrMax:
		return append(dst, mpFixStrBase|byte(size)) //nolint:gosec // The branch bounds size to five bits.
	case size <= math.MaxUint8:
		return append(dst, mpStr8, byte(size))
	case size <= math.MaxUint16:
		dst = append(dst, mpStr16)

		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(size))

		return append(dst, encoded[:]...)
	default:
		dst = append(dst, mpStr32)

		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], uint32(size)) //nolint:gosec // MessagePack values cannot exceed the input slice's int length.

		return append(dst, encoded[:]...)
	}
}

func appendMPBinary(dst, value []byte) []byte {
	size := len(value)

	switch {
	case size <= math.MaxUint8:
		dst = append(dst, mpBin8, byte(size))
	case size <= math.MaxUint16:
		dst = append(dst, mpBin16)

		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(size))
		dst = append(dst, encoded[:]...)
	default:
		dst = append(dst, mpBin32)

		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], uint32(size)) //nolint:gosec // MessagePack values cannot exceed the input slice's int length.
		dst = append(dst, encoded[:]...)
	}

	return append(dst, value...)
}

func appendMPArrayHeader(dst []byte, count int) []byte {
	switch {
	case count <= mpFixArrayMax:
		return append(dst, mpFixArrayBase|byte(count)) //nolint:gosec // The branch bounds count to four bits.
	case count <= math.MaxUint16:
		dst = append(dst, mpArray16)

		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(count))

		return append(dst, encoded[:]...)
	default:
		dst = append(dst, mpArray32)

		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], uint32(count)) //nolint:gosec // Decoded container counts fit int and MessagePack uint32.

		return append(dst, encoded[:]...)
	}
}

func appendMPMapHeader(dst []byte, count int) []byte {
	switch {
	case count <= mpFixMapMax:
		return append(dst, mpFixMapBase|byte(count)) //nolint:gosec // The branch bounds count to four bits.
	case count <= math.MaxUint16:
		dst = append(dst, mpMap16)

		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(count))

		return append(dst, encoded[:]...)
	default:
		dst = append(dst, mpMap32)

		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], uint32(count)) //nolint:gosec // Decoded container counts fit int and MessagePack uint32.

		return append(dst, encoded[:]...)
	}
}

func mpPayload(raw []byte, ext bool) ([]byte, error) {
	headerSize, payloadSize, err := mpPayloadLayout(raw, ext)
	if err != nil {
		return nil, err
	}

	if len(raw) != headerSize+payloadSize {
		return nil, fmt.Errorf("%w: payload size %d does not match %d bytes",
			ErrInvalidPrimaryKey, payloadSize, len(raw)-headerSize)
	}

	return raw[headerSize:], nil
}

func mpPayloadLayout(raw []byte, ext bool) (int, int, error) {
	if len(raw) == 0 {
		return 0, 0, fmt.Errorf("%w: empty payload", ErrInvalidPrimaryKey)
	}

	typeSize := 0
	if ext {
		typeSize = 1
	}

	switch raw[0] {
	case mpBin8, mpExt8:
		if len(raw) < mpLength8HeaderSize+typeSize {
			return 0, 0, fmt.Errorf("%w: truncated 8-bit payload header", ErrInvalidPrimaryKey)
		}

		return mpLength8HeaderSize + typeSize, int(raw[1]), nil
	case mpBin16, mpExt16:
		if len(raw) < mpLength16HeaderSize+typeSize {
			return 0, 0, fmt.Errorf("%w: truncated 16-bit payload header", ErrInvalidPrimaryKey)
		}

		return mpLength16HeaderSize + typeSize,
			int(binary.BigEndian.Uint16(raw[1:mpLength16HeaderSize])), nil
	case mpBin32, mpExt32:
		if len(raw) < mpLength32HeaderSize+typeSize {
			return 0, 0, fmt.Errorf("%w: truncated 32-bit payload header", ErrInvalidPrimaryKey)
		}

		size := binary.BigEndian.Uint32(raw[1:mpLength32HeaderSize])
		if uint64(size) > uint64(math.MaxInt) {
			return 0, 0, fmt.Errorf("%w: payload length overflows int", ErrInvalidPrimaryKey)
		}

		return mpLength32HeaderSize + typeSize, int(size), nil
	default:
		return 0, 0, fmt.Errorf("%w: invalid payload tag 0x%02x", ErrInvalidPrimaryKey, raw[0])
	}
}

func appendCanonicalExt(dst, raw []byte) ([]byte, error) {
	headerSize, payloadSize, err := mpExtLayout(raw)
	if err != nil {
		return nil, err
	}

	if len(raw) != headerSize+payloadSize {
		return nil, fmt.Errorf("%w: extension size %d does not match %d bytes",
			ErrInvalidPrimaryKey, payloadSize, len(raw)-headerSize)
	}

	typeCode := raw[headerSize-1]
	payload := raw[headerSize:]

	switch payloadSize {
	case mpFixExt1PayloadSize:
		dst = append(dst, mpFixExt1, typeCode)
	case mpFixExt2PayloadSize:
		dst = append(dst, mpFixExt2, typeCode)
	case mpFixExt4PayloadSize:
		dst = append(dst, mpFixExt4, typeCode)
	case mpFixExt8PayloadSize:
		dst = append(dst, mpFixExt8, typeCode)
	case mpFixExt16PayloadSize:
		dst = append(dst, mpFixExt16, typeCode)
	default:
		switch {
		case payloadSize <= math.MaxUint8:
			dst = append(dst, mpExt8, byte(payloadSize), typeCode) //nolint:gosec // The branch bounds the extension length to uint8.
		case payloadSize <= math.MaxUint16:
			dst = append(dst, mpExt16)

			var encoded [2]byte
			binary.BigEndian.PutUint16(encoded[:], uint16(payloadSize))
			dst = append(dst, encoded[:]...)
			dst = append(dst, typeCode)
		default:
			dst = append(dst, mpExt32)

			var encoded [4]byte
			binary.BigEndian.PutUint32(encoded[:], uint32(payloadSize)) //nolint:gosec // Payload is bounded by the input slice's int length.
			dst = append(dst, encoded[:]...)
			dst = append(dst, typeCode)
		}
	}

	return append(dst, payload...), nil
}

func mpExtLayout(raw []byte) (int, int, error) {
	if len(raw) == 0 {
		return 0, 0, fmt.Errorf("%w: empty extension", ErrInvalidPrimaryKey)
	}

	switch raw[0] {
	case mpFixExt1:
		return mpFixExtHeaderSize, mpFixExt1PayloadSize, nil
	case mpFixExt2:
		return mpFixExtHeaderSize, mpFixExt2PayloadSize, nil
	case mpFixExt4:
		return mpFixExtHeaderSize, mpFixExt4PayloadSize, nil
	case mpFixExt8:
		return mpFixExtHeaderSize, mpFixExt8PayloadSize, nil
	case mpFixExt16:
		return mpFixExtHeaderSize, mpFixExt16PayloadSize, nil
	case mpExt8, mpExt16, mpExt32:
		return mpPayloadLayout(raw, true)
	default:
		return 0, 0, fmt.Errorf("%w: invalid extension tag 0x%02x", ErrInvalidPrimaryKey, raw[0])
	}
}
