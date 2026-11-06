package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// This file decodes the operations of an UPDATE or UPSERT, after Tarantool's
// xrow_update_op_decode and xrow_update_op_by (src/box/xrow_update_field.c).
// The operations are a msgpack array of operations, each an array of an
// opcode, a field and the arguments of the opcode:
//
//	[op, field, value]                   = ! + - & | ^ #
//	[op, field, position, offset, value] :
//
// Tarantool takes exactly 3 elements for every opcode but splice, which takes
// 5 (the arg_count of the op_* metas); there is no 2-element shape. Delete
// is [#, field, count] too: its third element is the number of fields to
// delete, a positive integer (xrow_update_read_arg_delete).

// OpCode is the opcode of an update operation: the one-character string an
// operation array starts with.
type OpCode byte

// The opcodes Tarantool knows (xrow_update_op_by).
const (
	// OpSet assigns Value to the field: [=, field, value].
	OpSet OpCode = '='
	// OpInsert inserts Value before the field: [!, field, value].
	OpInsert OpCode = '!'
	// OpAdd adds the number Value to the field: [+, field, value].
	OpAdd OpCode = '+'
	// OpSubtract subtracts the number Value from the field: [-, field, value].
	OpSubtract OpCode = '-'
	// OpAnd is the bitwise AND of the field and the unsigned Value:
	// [&, field, value].
	OpAnd OpCode = '&'
	// OpOr is the bitwise OR of the field and the unsigned Value:
	// [|, field, value].
	OpOr OpCode = '|'
	// OpXor is the bitwise XOR of the field and the unsigned Value:
	// [^, field, value].
	OpXor OpCode = '^'
	// OpSplice replaces Offset bytes of the string field at Position with the
	// string Value: [:, field, position, offset, value].
	OpSplice OpCode = ':'
	// OpDelete deletes Value fields starting with the field, Value being a
	// positive integer: [#, field, count].
	OpDelete OpCode = '#'
)

// The number of elements of an operation array (the arg_count of
// Tarantool's op_* metas).
const (
	opArity       = 3 // [op, field, value]: every opcode but splice.
	opSpliceArity = 5 // [op, field, position, offset, value].

	// opMinSize is the size of the smallest valid operation: a fixarray, a
	// one-character fixstr opcode, a fixint field and a one-byte value. It
	// bounds the capacity DecodeOps preallocates for a hostile count.
	opMinSize = mpTagSize + mpHead8 + mpTagSize + mpTagSize
)

// String returns the opcode as Tarantool writes it, "=" for OpSet, or
// OpCode(0x..) for a byte that is no opcode.
func (c OpCode) String() string {
	switch c {
	case OpSet:
		return "="
	case OpInsert:
		return "!"
	case OpAdd:
		return "+"
	case OpSubtract:
		return "-"
	case OpAnd:
		return "&"
	case OpOr:
		return "|"
	case OpXor:
		return "^"
	case OpSplice:
		return ":"
	case OpDelete:
		return "#"
	default:
		return fmt.Sprintf("OpCode(0x%02x)", byte(c))
	}
}

// arity returns the number of elements of an operation of the opcode, or 0
// for a byte that is no opcode.
func (c OpCode) arity() int {
	switch c {
	case OpSet, OpInsert, OpAdd, OpSubtract, OpAnd, OpOr, OpXor, OpDelete:
		return opArity
	case OpSplice:
		return opSpliceArity
	default:
		return 0
	}
}

// Operation is a decoded update operation. FieldName and Value alias the
// bytes it was decoded from.
//
// Field numbers and the splice position are as written in the request.
// Tarantool subtracts the index base of the request from a non-negative one:
// IPROTO_INDEX_BASE (key 0x15, in DMLBody.Extras), which box.space:update and
// :upsert set to 1 and which is 0 when absent. A negative one counts from the
// end, -1 being the last field. The operations of a vinyl run
// (VyStmtBody.Ops) are 0-based.
type Operation struct {
	// Op is the opcode.
	Op OpCode
	// Field is the number of the field the operation addresses when
	// FieldName is nil, and 0 otherwise.
	Field int32
	// FieldName is the name or JSON path of the field the operation
	// addresses, nil when it addresses it by number. It aliases the input.
	// Tarantool writes a string; a bin is accepted too.
	FieldName []byte
	// HasSplice reports that Position and Offset are set: the operation is
	// a splice.
	HasSplice bool
	// Position is the position in the string of a splice where Offset bytes
	// are cut and Value pasted (Tarantool's offset); a negative one counts
	// from the end of the string.
	Position int32
	// Offset is the number of bytes a splice cuts (Tarantool's cut_length).
	Offset int32
	// Value is the raw msgpack of the last element of the operation: the
	// value of =, !, +, -, &, |, ^, the number of fields of # and the string
	// : pastes. It aliases the input.
	Value []byte
}

// Sentinel errors of DecodeOps and DecodeOpsInto, wrapped with the position
// of the operation at fault.
var (
	// ErrOpNotArray is returned when the operations, or one of them, are not
	// a msgpack array.
	ErrOpNotArray = errors.New("not an array")
	// ErrOpArity is returned when an operation has the wrong number of
	// elements for its opcode.
	ErrOpArity = errors.New("wrong number of arguments")
	// ErrOpCodeType is returned when the opcode of an operation is not a
	// string.
	ErrOpCodeType = errors.New("opcode is not a string")
	// ErrUnknownOpCode is returned when the opcode of an operation is not
	// one of Tarantool's.
	ErrUnknownOpCode = errors.New("unknown opcode")
	// ErrOpFieldType is returned when the field of an operation is neither
	// an integer nor a string.
	ErrOpFieldType = errors.New("field is neither an integer nor a string")
	// ErrOpArgType is returned when the position or offset of a splice is
	// not an integer.
	ErrOpArgType = errors.New("splice argument is not an integer")
	// ErrOpIntRange is returned when a field number or a splice argument does
	// not fit an int32, as Tarantool requires (mp_read_int32).
	ErrOpIntRange = errors.New("integer out of int32 range")
)

// DecodeOps decodes the operations of an UPDATE or UPSERT: the IPROTO_TUPLE
// of an UPDATE (DMLBody.Tuple - Tarantool keeps the UPDATE operations under
// IPROTO_TUPLE for legacy reasons, src/box/iproto_constants.h) or the
// IPROTO_OPS of an UPSERT (DMLBody.Ops). The Ops of a vinyl UPSERT
// (VyStmtBody.Ops) is an array of such operation lists; DecodeOps decodes one
// list.
//
// It checks the shape of each operation - its number of elements and the
// types of its opcode, field and splice arguments - not the type of its
// value, which Tarantool checks when it applies the operation. Bytes after
// the array are ignored. The FieldName and Value of every operation alias b.
func DecodeOps(b []byte) ([]Operation, error) {
	n, off, err := readOpsLen(b)
	if err != nil {
		return nil, err
	}

	// Every operation takes at least opMinSize bytes: do not trust a count
	// the input cannot hold.
	ops, err := decodeOps(b, off, n, make([]Operation, 0, min(n, (len(b)-off)/opMinSize)))
	if err != nil {
		return nil, err
	}

	return ops, nil
}

// DecodeOpsInto is DecodeOps decoding into dst[:0], reusing its capacity: it
// does not allocate when cap(dst) holds every operation. It returns the
// operations, or dst[:0] and an error. The FieldName and Value of every
// operation alias b.
func DecodeOpsInto(b []byte, dst []Operation) ([]Operation, error) {
	n, off, err := readOpsLen(b)
	if err != nil {
		return dst[:0], err
	}

	return decodeOps(b, off, n, dst[:0])
}

// readOpsLen reads the array header of the operations.
func readOpsLen(b []byte) (int, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("format: DecodeOps: %w", ErrEmptyInput)
	}

	if !isMPArray(b[0]) {
		return 0, 0, fmt.Errorf("format: DecodeOps: operations: %w: tag 0x%02x", ErrOpNotArray, b[0])
	}

	n, off, err := readMPArrayLen(b)
	if err != nil {
		return 0, 0, fmt.Errorf("format: DecodeOps: %w", err)
	}

	return n, off, nil
}

// decodeOps appends the n operations at b[off:] to dst. On error it returns
// dst[:0].
func decodeOps(b []byte, off, n int, dst []Operation) ([]Operation, error) {
	var zero Operation

	for i := range n {
		dst = append(dst, zero)

		m, err := decodeOp(b[off:], &dst[len(dst)-1])
		if err != nil {
			return dst[:0], fmt.Errorf("format: DecodeOps: op %d: %w", i, err)
		}

		off += m
	}

	return dst, nil
}

// decodeOp decodes the operation at b[0:] into op, after
// xrow_update_op_decode, and returns the bytes it takes.
func decodeOp(b []byte, op *Operation) (int, error) {
	if len(b) == 0 {
		return 0, ErrTruncatedInput
	}

	if !isMPArray(b[0]) {
		return 0, fmt.Errorf("%w: tag 0x%02x", ErrOpNotArray, b[0])
	}

	argc, off, err := readMPArrayLen(b)
	if err != nil {
		return 0, err
	}

	if argc == 0 {
		return 0, fmt.Errorf("%w: empty operation", ErrOpArity)
	}

	if off >= len(b) {
		return 0, fmt.Errorf("opcode: %w", ErrTruncatedInput)
	}

	if !isMPStr(b[off]) {
		return 0, fmt.Errorf("%w: tag 0x%02x", ErrOpCodeType, b[off])
	}

	code, n, err := readMPStrBin(b[off:])
	if err != nil {
		return 0, fmt.Errorf("opcode: %w", err)
	}

	off += n

	if len(code) != 1 || OpCode(code[0]).arity() == 0 {
		return 0, fmt.Errorf("%w %q", ErrUnknownOpCode, code)
	}

	op.Op = OpCode(code[0])

	if want := op.Op.arity(); argc != want {
		return 0, fmt.Errorf("%w: %q takes %d, got %d", ErrOpArity, code, want, argc)
	}

	if off >= len(b) {
		return 0, fmt.Errorf("field: %w", ErrTruncatedInput)
	}

	switch c := b[off]; {
	case isMPInt(c):
		op.Field, n, err = readMPInt32(b[off:])
		if err != nil {
			return 0, fmt.Errorf("field: %w", err)
		}
	case isMPStr(c), isMPBin(c):
		op.FieldName, n, err = readMPStrBin(b[off:])
		if err != nil {
			return 0, fmt.Errorf("field: %w", err)
		}
	default:
		return 0, fmt.Errorf("%w: tag 0x%02x", ErrOpFieldType, c)
	}

	off += n

	if op.Op == OpSplice {
		op.Position, n, err = readSpliceArg(b[off:])
		if err != nil {
			return 0, fmt.Errorf("splice position: %w", err)
		}

		off += n

		op.Offset, n, err = readSpliceArg(b[off:])
		if err != nil {
			return 0, fmt.Errorf("splice offset: %w", err)
		}

		off += n
		op.HasSplice = true
	}

	if off >= len(b) {
		return 0, fmt.Errorf("value: %w", ErrTruncatedInput)
	}

	n, err = skipMP(b[off:])
	if err != nil {
		return 0, fmt.Errorf("value: %w", err)
	}

	op.Value = b[off : off+n : off+n]

	return off + n, nil
}

// readSpliceArg reads the position or offset of a splice.
func readSpliceArg(b []byte) (int32, int, error) {
	if len(b) == 0 {
		return 0, 0, ErrTruncatedInput
	}

	if !isMPInt(b[0]) {
		return 0, 0, fmt.Errorf("%w: tag 0x%02x", ErrOpArgType, b[0])
	}

	return readMPInt32(b)
}

// isMPArray reports whether c is the tag of a msgpack array.
func isMPArray(c byte) bool {
	return (c >= mpcFixedArrayLow && c <= mpcFixedArrayHigh) || c == mpcArray16 || c == mpcArray32
}

// isMPStr reports whether c is the tag of a msgpack str.
func isMPStr(c byte) bool {
	return (c >= mpcFixedStrLow && c <= mpcFixedStrHigh) || c == mpcStr8 || c == mpcStr16 || c == mpcStr32
}

// isMPBin reports whether c is the tag of a msgpack bin.
func isMPBin(c byte) bool {
	return c == mpcBin8 || c == mpcBin16 || c == mpcBin32
}

// isMPInt reports whether c is the tag of a msgpack integer, signed or not.
func isMPInt(c byte) bool {
	switch {
	case c <= mpcPosFixedNumHigh, c >= mpcNegFixedNumLow:
		return true
	}

	switch c {
	case mpcUint8, mpcUint16, mpcUint32, mpcUint64, mpcInt8, mpcInt16, mpcInt32, mpcInt64:
		return true
	default:
		return false
	}
}

// readMPInt32 reads a msgpack integer of any width that fits an int32, as
// Tarantool's mp_read_int32, and returns (value, bytes-read).
func readMPInt32(b []byte) (int32, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("format: mp int: %w", ErrEmptyInput)
	}

	var (
		v int64
		n int
	)

	switch c := b[0]; c {
	case mpcInt8:
		if len(b) < mpHead8 {
			return 0, 0, fmt.Errorf("format: mp int8: %w", ErrShortInput)
		}

		v, n = int64(int8(b[1])), mpHead8 //nolint:gosec // G115: msgpack int8 is a two's complement byte
	case mpcInt16:
		if len(b) < mpHead16 {
			return 0, 0, fmt.Errorf("format: mp int16: %w", ErrShortInput)
		}

		v, n = int64(int16(binary.BigEndian.Uint16(b[1:mpHead16]))), mpHead16 //nolint:gosec // G115: msgpack int16 is two's complement
	case mpcInt32:
		if len(b) < mpHead32 {
			return 0, 0, fmt.Errorf("format: mp int32: %w", ErrShortInput)
		}

		v, n = int64(int32(binary.BigEndian.Uint32(b[1:mpHead32]))), mpHead32 //nolint:gosec // G115: msgpack int32 is two's complement
	case mpcInt64:
		if len(b) < mpHead64 {
			return 0, 0, fmt.Errorf("format: mp int64: %w", ErrShortInput)
		}

		v, n = int64(binary.BigEndian.Uint64(b[1:mpHead64])), mpHead64 //nolint:gosec // G115: msgpack int64 is two's complement
	default:
		if c >= mpcNegFixedNumLow {
			v, n = int64(int8(c)), mpTagSize //nolint:gosec // G115: a negative fixint is a two's complement byte

			break
		}

		u, m, err := readMPUint(b)
		if err != nil {
			return 0, 0, err
		}

		if u > math.MaxInt32 {
			return 0, 0, fmt.Errorf("%w: %d", ErrOpIntRange, u)
		}

		v, n = int64(u), m
	}

	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, 0, fmt.Errorf("%w: %d", ErrOpIntRange, v)
	}

	return int32(v), n, nil
}

// readMPStrBin reads a msgpack str or bin and returns (payload, bytes-read).
// The payload aliases b and is non-nil, even when empty.
func readMPStrBin(b []byte) ([]byte, int, error) {
	if len(b) == 0 {
		return nil, 0, fmt.Errorf("format: mp str: %w", ErrEmptyInput)
	}

	var (
		l    uint64
		head int
	)

	switch c := b[0]; {
	case c >= mpcFixedStrLow && c <= mpcFixedStrHigh:
		l, head = uint64(c&mpcFixedStrMask), mpTagSize
	case c == mpcStr8 || c == mpcBin8:
		if len(b) < mpHead8 {
			return nil, 0, fmt.Errorf("format: mp str8/bin8: %w", ErrShortInput)
		}

		l, head = uint64(b[1]), mpHead8
	case c == mpcStr16 || c == mpcBin16:
		if len(b) < mpHead16 {
			return nil, 0, fmt.Errorf("format: mp str16/bin16: %w", ErrShortInput)
		}

		l, head = uint64(binary.BigEndian.Uint16(b[1:mpHead16])), mpHead16
	case c == mpcStr32 || c == mpcBin32:
		if len(b) < mpHead32 {
			return nil, 0, fmt.Errorf("format: mp str32/bin32: %w", ErrShortInput)
		}

		l, head = uint64(binary.BigEndian.Uint32(b[1:mpHead32])), mpHead32
	default:
		return nil, 0, fmt.Errorf("format: mp str: %w 0x%02x", ErrUnexpectedTag, c)
	}

	if l > uint64(len(b)-head) { //nolint:gosec // G115: head <= len(b), checked above
		return nil, 0, fmt.Errorf("format: mp str: %w", ErrShortInput)
	}

	end := head + int(l) // l <= len(b) - head, checked above.

	return b[head:end:end], end, nil
}
