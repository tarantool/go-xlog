package format_test

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
)

// opArr returns a msgpack array of the encoded elems.
func opArr(elems ...[]byte) []byte {
	b := make([]byte, 0, 64)
	if len(elems) < 16 {
		b = append(b, 0x90|byte(len(elems)))
	} else {
		b = append(b, 0xdc, byte(len(elems)>>8), byte(len(elems)))
	}

	for _, e := range elems {
		b = append(b, e...)
	}

	return b
}

// opStrOrBin returns s as a msgpack str (bin false) or bin (bin true) of the
// header width: 0 for fixstr, 8, 16 or 32.
func opStrOrBin(s string, width int, bin bool) []byte {
	b := make([]byte, 0, len(s)+5)

	switch width {
	case 0:
		b = append(b, 0xa0|byte(len(s)))
	case 8:
		b = append(b, 0xd9, byte(len(s)))
	case 16:
		b = binary.BigEndian.AppendUint16(append(b, 0xda), uint16(len(s)))
	default:
		b = binary.BigEndian.AppendUint32(append(b, 0xdb), uint32(len(s)))
	}

	if bin {
		// The str8/16/32 tags are 0xd9-0xdb; the bin ones 0xc4-0xc6.
		b[0] -= 0xd9 - 0xc4
	}

	return append(b, s...)
}

// opS returns s as a fixstr.
func opS(s string) []byte { return opStrOrBin(s, 0, false) }

// opInt returns a msgpack integer of the given tag and big-endian width.
func opInt(tag byte, v uint64, width int) []byte {
	b := []byte{tag}
	for i := width - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}

	return b
}

func TestOpCode_String(t *testing.T) {
	t.Parallel()

	for _, c := range []format.OpCode{
		format.OpSet, format.OpInsert, format.OpAdd, format.OpSubtract, format.OpAnd,
		format.OpOr, format.OpXor, format.OpSplice, format.OpDelete,
	} {
		assert.Equal(t, string(rune(c)), c.String())
	}

	assert.Equal(t, "OpCode(0x78)", format.OpCode('x').String())
}

func TestDecodeOps_Shapes(t *testing.T) {
	t.Parallel()

	value := opS("v")

	tests := []struct {
		name string
		in   []byte
		want format.Operation
	}{
		{"set", opArr(opS("="), []byte{2}, value), format.Operation{Op: format.OpSet, Field: 2, Value: value}},
		{"insert", opArr(opS("!"), []byte{2}, value), format.Operation{Op: format.OpInsert, Field: 2, Value: value}},
		{"add", opArr(opS("+"), []byte{3}, []byte{5}), format.Operation{Op: format.OpAdd, Field: 3, Value: []byte{5}}},
		{"add double", opArr(opS("+"), []byte{3}, opInt(0xcb, math.Float64bits(1.5), 8)), format.Operation{Op: format.OpAdd, Field: 3, Value: opInt(0xcb, math.Float64bits(1.5), 8)}},
		{"subtract", opArr(opS("-"), []byte{3}, []byte{0xff}), format.Operation{Op: format.OpSubtract, Field: 3, Value: []byte{0xff}}},
		{"and", opArr(opS("&"), []byte{4}, []byte{6}), format.Operation{Op: format.OpAnd, Field: 4, Value: []byte{6}}},
		{"or", opArr(opS("|"), []byte{4}, []byte{6}), format.Operation{Op: format.OpOr, Field: 4, Value: []byte{6}}},
		{"xor", opArr(opS("^"), []byte{4}, []byte{6}), format.Operation{Op: format.OpXor, Field: 4, Value: []byte{6}}},
		{"delete count", opArr(opS("#"), []byte{5}, []byte{2}), format.Operation{Op: format.OpDelete, Field: 5, Value: []byte{2}}},
		{"set array value", opArr(opS("="), []byte{1}, opArr([]byte{1}, opS("ab"))), format.Operation{Op: format.OpSet, Field: 1, Value: opArr([]byte{1}, opS("ab"))}},
		{
			"splice", opArr(opS(":"), []byte{2}, []byte{3}, []byte{1}, opS("xy")),
			format.Operation{Op: format.OpSplice, Field: 2, HasSplice: true, Position: 3, Offset: 1, Value: opS("xy")},
		},
		{
			"splice negative position", opArr(opS(":"), []byte{0xff}, []byte{0xfe}, opInt(0xcd, 300, 2), opS("")),
			format.Operation{Op: format.OpSplice, Field: -1, HasSplice: true, Position: -2, Offset: 300, Value: opS("")},
		},
		{
			"opcode str8", opArr(opStrOrBin("=", 8, false), []byte{1}, value),
			format.Operation{Op: format.OpSet, Field: 1, Value: value},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ops, err := format.DecodeOps(opArr(tc.in))
			require.NoError(t, err)
			require.Equal(t, []format.Operation{tc.want}, ops)
		})
	}
}

func TestDecodeOps_FieldNumber(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field []byte
		want  int32
	}{
		{"fixint 0", []byte{0x00}, 0},
		{"fixint 127", []byte{0x7f}, 127},
		{"uint8", opInt(0xcc, 200, 1), 200},
		{"uint16", opInt(0xcd, 60000, 2), 60000},
		{"uint32", opInt(0xce, math.MaxInt32, 4), math.MaxInt32},
		{"uint64", opInt(0xcf, 7, 8), 7},
		{"negative fixint -1", []byte{0xff}, -1},
		{"negative fixint -32", []byte{0xe0}, -32},
		{"int8", opInt(0xd0, uint64(0x9c), 1), -100},
		{"int8 positive", opInt(0xd0, 5, 1), 5},
		{"int16", opInt(0xd1, uint64(uint16(0x8000)), 2), math.MinInt16},
		{"int32", opInt(0xd2, uint64(uint32(0x80000000)), 4), math.MinInt32},
		{"int64", opInt(0xd3, math.MaxUint64-4, 8), -5},
		{"int64 positive", opInt(0xd3, math.MaxInt32, 8), math.MaxInt32},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ops, err := format.DecodeOps(opArr(opArr(opS("="), tc.field, []byte{0xc0})))
			require.NoError(t, err)
			require.Len(t, ops, 1)
			assert.Equal(t, tc.want, ops[0].Field)
			assert.Nil(t, ops[0].FieldName, "a field number has no name")
			assert.Equal(t, []byte{0xc0}, ops[0].Value)
		})
	}
}

func TestDecodeOps_FieldName(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("f", 300)

	tests := []struct {
		name  string
		field []byte
		want  string
	}{
		{"fixstr", opS("name"), "name"},
		{"json path", opS("[2].a.b"), "[2].a.b"},
		{"empty", opS(""), ""},
		{"str8", opStrOrBin("name", 8, false), "name"},
		{"str16", opStrOrBin(long, 16, false), long},
		{"str32", opStrOrBin("name", 32, false), "name"},
		{"bin8", opStrOrBin("name", 8, true), "name"},
		{"bin16", opStrOrBin(long, 16, true), long},
		{"bin32", opStrOrBin("name", 32, true), "name"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := opArr(opArr(opS("="), tc.field, []byte{1}))

			ops, err := format.DecodeOps(in)
			require.NoError(t, err)
			require.Len(t, ops, 1)
			require.NotNil(t, ops[0].FieldName, "a field name is never nil")
			assert.Equal(t, tc.want, string(ops[0].FieldName))
			assert.Zero(t, ops[0].Field)
			assert.Equal(t, []byte{1}, ops[0].Value)

			// FieldName aliases the input.
			if len(tc.want) > 0 {
				at := bytes.Index(in, []byte(tc.want))
				assert.Same(t, &in[at], &ops[0].FieldName[0])
			}
		})
	}
}

func TestDecodeOps_Errors(t *testing.T) {
	t.Parallel()

	v := []byte{1}

	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty input", nil, format.ErrEmptyInput},
		{"ops not array", []byte{0x80}, format.ErrOpNotArray},
		{"ops str", opS("="), format.ErrOpNotArray},
		{"ops truncated header", []byte{0xdc, 0x00}, format.ErrShortInput},
		{"op not array", opArr(opS("=")), format.ErrOpNotArray},
		{"op map", opArr([]byte{0x81, 0x01, 0x02}), format.ErrOpNotArray},
		{"op empty array", opArr(opArr()), format.ErrOpArity},
		{"set of 2", opArr(opArr(opS("="), v)), format.ErrOpArity},
		{"set of 4", opArr(opArr(opS("="), v, v, v)), format.ErrOpArity},
		{"set of 5", opArr(opArr(opS("="), v, v, v, v)), format.ErrOpArity},
		{"delete of 2", opArr(opArr(opS("#"), v)), format.ErrOpArity},
		{"splice of 3", opArr(opArr(opS(":"), v, opS("x"))), format.ErrOpArity},
		{"opcode only", opArr(opArr(opS("+"))), format.ErrOpArity},
		{"opcode int", opArr(opArr([]byte{0x3d}, v, v)), format.ErrOpCodeType},
		{"opcode bin", opArr(opArr(opStrOrBin("=", 8, true), v, v)), format.ErrOpCodeType},
		{"opcode nil", opArr(opArr([]byte{0xc0}, v, v)), format.ErrOpCodeType},
		{"opcode unknown", opArr(opArr(opS("x"), v, v)), format.ErrUnknownOpCode},
		{"opcode two chars", opArr(opArr(opS("=="), v, v)), format.ErrUnknownOpCode},
		{"opcode empty", opArr(opArr(opS(""), v, v)), format.ErrUnknownOpCode},
		{"field nil", opArr(opArr(opS("="), []byte{0xc0}, v)), format.ErrOpFieldType},
		{"field bool", opArr(opArr(opS("="), []byte{0xc3}, v)), format.ErrOpFieldType},
		{"field double", opArr(opArr(opS("="), opInt(0xcb, 0, 8), v)), format.ErrOpFieldType},
		{"field array", opArr(opArr(opS("="), opArr(v), v)), format.ErrOpFieldType},
		{"field over int32", opArr(opArr(opS("="), opInt(0xce, math.MaxInt32+1, 4), v)), format.ErrOpIntRange},
		{"field over int64", opArr(opArr(opS("="), opInt(0xcf, math.MaxUint64, 8), v)), format.ErrOpIntRange},
		{"field under int32", opArr(opArr(opS("="), opInt(0xd3, uint64(math.MaxUint64-math.MaxInt32-1), 8), v)), format.ErrOpIntRange},
		{"field truncated int", []byte{0x91, 0x93, 0xa1, '=', 0xcd, 0x01}, format.ErrShortInput},
		{"field truncated str", []byte{0x91, 0x93, 0xa1, '=', 0xa5, 'a'}, format.ErrShortInput},
		{"field truncated int8", []byte{0x91, 0x93, 0xa1, '=', 0xd0}, format.ErrShortInput},
		{"field truncated int16", []byte{0x91, 0x93, 0xa1, '=', 0xd1, 0x00}, format.ErrShortInput},
		{"field truncated int32", []byte{0x91, 0x93, 0xa1, '=', 0xd2, 0x00, 0x00}, format.ErrShortInput},
		{"field truncated int64", []byte{0x91, 0x93, 0xa1, '=', 0xd3, 0x00}, format.ErrShortInput},
		{"field int64 over int32", opArr(opArr(opS("="), opInt(0xd3, math.MaxInt32+1, 8), v)), format.ErrOpIntRange},
		{"field truncated str8", []byte{0x91, 0x93, 0xa1, '=', 0xd9}, format.ErrShortInput},
		{"field truncated bin16", []byte{0x91, 0x93, 0xa1, '=', 0xc5, 0x00}, format.ErrShortInput},
		{"field truncated str32", []byte{0x91, 0x93, 0xa1, '=', 0xdb, 0x00, 0x00}, format.ErrShortInput},
		{"missing opcode", []byte{0x91, 0x93}, format.ErrTruncatedInput},
		{"truncated opcode", []byte{0x91, 0x93, 0xa3, '='}, format.ErrShortInput},
		{"missing field", []byte{0x91, 0x93, 0xa1, '='}, format.ErrTruncatedInput},
		{"splice missing position", []byte{0x91, 0x95, 0xa1, ':', 0x01}, format.ErrTruncatedInput},
		{"splice truncated offset", []byte{0x91, 0x95, 0xa1, ':', 0x01, 0x01, 0xd2, 0x00}, format.ErrShortInput},
		{"splice position str", opArr(opArr(opS(":"), v, opS("1"), v, opS("x"))), format.ErrOpArgType},
		{"splice offset nil", opArr(opArr(opS(":"), v, v, []byte{0xc0}, opS("x"))), format.ErrOpArgType},
		{"splice position over int32", opArr(opArr(opS(":"), v, opInt(0xcf, 1<<40, 8), v, opS("x"))), format.ErrOpIntRange},
		{"missing value", opArr([]byte{0x93}, opS("="), v), format.ErrTruncatedInput},
		{"truncated value", opArr(opArr(opS("="), v, []byte{0xa3, 'a'})), format.ErrShortFixstr},
		{"missing op", []byte{0x92, 0x93, 0xa1, '=', 0x01, 0x01}, format.ErrTruncatedInput},
		{"hostile count", []byte{0xdd, 0xff, 0xff, 0xff, 0xff, 0x93}, format.ErrTruncatedInput},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ops, err := format.DecodeOps(tc.in)
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, "format: DecodeOps: ")
			assert.Nil(t, ops)

			dst := make([]format.Operation, 0, 4)

			got, err := format.DecodeOpsInto(tc.in, dst)
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, got)
			assert.Equal(t, cap(dst), cap(got), "DecodeOpsInto keeps the capacity of dst on error")
		})
	}
}

// TestDecodeOps_ErrorPosition checks the error names the operation at fault.
func TestDecodeOps_ErrorPosition(t *testing.T) {
	t.Parallel()

	in := opArr(opArr(opS("="), []byte{1}, []byte{1}), opArr(opS("?"), []byte{1}, []byte{1}))

	_, err := format.DecodeOps(in)
	require.ErrorIs(t, err, format.ErrUnknownOpCode)
	assert.EqualError(t, err, `format: DecodeOps: op 1: unknown opcode "?"`)
}

func TestDecodeOps_Empty(t *testing.T) {
	t.Parallel()

	ops, err := format.DecodeOps([]byte{0x90})
	require.NoError(t, err)
	assert.Empty(t, ops)

	// Bytes after the array are ignored.
	ops, err = format.DecodeOps(append(opArr(opArr(opS("="), []byte{1}, []byte{2})), 0xc1))
	require.NoError(t, err)
	assert.Len(t, ops, 1)
}

// TestDecodeOps_Msgpack decodes operations a general msgpack encoder writes,
// with compact and full-width integers.
func TestDecodeOps_Msgpack(t *testing.T) {
	t.Parallel()

	src := []any{
		[]any{"=", 2, "alpha"},
		[]any{"!", -1, []any{1, "x"}},
		[]any{"+", 3, 2.5},
		[]any{"-", int64(-70000), uint64(1) << 40},
		[]any{"&", 1, uint8(0x0f)},
		[]any{"|", 1, 0x100},
		[]any{"^", 1, 1},
		[]any{":", "name", -3, 2, "xy"},
		[]any{"#", []byte("bin.field"), 2},
	}
	want := []format.Operation{
		{Op: format.OpSet, Field: 2},
		{Op: format.OpInsert, Field: -1},
		{Op: format.OpAdd, Field: 3},
		{Op: format.OpSubtract, Field: -70000},
		{Op: format.OpAnd, Field: 1},
		{Op: format.OpOr, Field: 1},
		{Op: format.OpXor, Field: 1},
		{Op: format.OpSplice, FieldName: []byte("name"), HasSplice: true, Position: -3, Offset: 2},
		{Op: format.OpDelete, FieldName: []byte("bin.field")},
	}

	for _, compact := range []bool{true, false} {
		var buf bytes.Buffer

		enc := msgpack.NewEncoder(&buf)
		enc.UseCompactInts(compact)
		require.NoError(t, enc.Encode(src))

		ops, err := format.DecodeOps(buf.Bytes())
		require.NoError(t, err, "compact=%v", compact)
		require.Len(t, ops, len(src))

		for i, op := range ops {
			// Value is the encoding of the last element.
			elems, ok := src[i].([]any)
			require.True(t, ok)

			var value bytes.Buffer

			enc := msgpack.NewEncoder(&value)
			enc.UseCompactInts(compact)
			require.NoError(t, enc.Encode(elems[len(elems)-1]))
			assert.Equal(t, value.Bytes(), op.Value, "op %d value (compact=%v)", i, compact)

			op.Value = nil
			assert.Equal(t, want[i], op, "op %d (compact=%v)", i, compact)
		}
	}
}

func TestDecodeOpsInto_Reuse(t *testing.T) {
	t.Parallel()

	splice := opArr(opArr(opS(":"), opS("f"), []byte{1}, []byte{2}, opS("x")), opArr(opS("="), []byte{1}, []byte{1}))
	set := opArr(opArr(opS("="), []byte{3}, []byte{4}))

	dst := make([]format.Operation, 0, 4)

	got, err := format.DecodeOpsInto(splice, dst)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Same(t, &dst[:1][0], &got[0], "DecodeOpsInto reuses the capacity of dst")

	// Nothing of the splice bleeds into the operation decoded over it.
	got, err = format.DecodeOpsInto(set, got)
	require.NoError(t, err)
	assert.Equal(t, []format.Operation{{Op: format.OpSet, Field: 3, Value: []byte{4}}}, got)

	// A dst too small grows.
	got, err = format.DecodeOpsInto(splice, nil)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// opsBench is a typical list of operations: a set by number, an add by name,
// a splice and a delete.
func opsBench() []byte {
	return opArr(
		opArr(opS("="), []byte{2}, opS("alpha")),
		opArr(opS("+"), opS("counter"), []byte{1}),
		opArr(opS(":"), []byte{3}, []byte{1}, []byte{2}, opS("xy")),
		opArr(opS("#"), []byte{0xff}, []byte{1}),
	)
}

//nolint:paralleltest // AllocsPerRun counts the allocations of every goroutine.
func TestDecodeOpsInto_ZeroAlloc(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation counting is skipped in -short mode")
	}

	in := opsBench()
	dst := make([]format.Operation, 0, 4)

	allocs := testing.AllocsPerRun(100, func() {
		var err error

		dst, err = format.DecodeOpsInto(in, dst)
		if err != nil || len(dst) != 4 {
			t.Fatalf("DecodeOpsInto: %v, %d ops", err, len(dst))
		}
	})
	assert.Zero(t, allocs, "DecodeOpsInto with a large enough dst allocates")
}

func BenchmarkDecodeOps(b *testing.B) {
	in := opsBench()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := format.DecodeOps(in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeOpsInto(b *testing.B) {
	in := opsBench()
	dst := make([]format.Operation, 0, 4)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		var err error

		dst, err = format.DecodeOpsInto(in, dst)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// fixtureRows returns the rows of the xlog-envelope fixture name.
func fixtureRows(t *testing.T, name string) []format.XRow {
	t.Helper()

	data := testutil.Load(t, name)

	_, err := format.DecodeMeta(bufio.NewReader(bytes.NewReader(data)), format.MetaOptions{})
	require.NoError(t, err)

	off := bytes.Index(data, []byte("\n\n")) + 2

	var rows []format.XRow

	for off < len(data) && !bytes.HasPrefix(data[off:], format.EOFMarker[:]) {
		slices, _, n, err := format.DecodeTxBlock(data[off:])
		require.NoError(t, err)

		for _, s := range slices {
			row, _, err := format.DecodeXRow(s)
			require.NoError(t, err)

			rows = append(rows, *row)
		}

		off += n
	}

	return rows
}

// TestDecodeOps_Fixtures decodes the operations real Tarantools wrote:
// s:update({1}, {{'=', 2, 'alpha2'}}) and s:upsert({3, 'gamma'}, {{'=', 2,
// 'gamma2'}}) of scripts/historical/gen-modern.lua, and the squashed vinyl
// UPSERTs of scripts/gen-vinyl.lua.
func TestDecodeOps_Fixtures(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"2.11", "3.0", "3.8"} {
		var update, upsert []format.Operation

		for _, row := range fixtureRows(t, "historical/"+version+"/00000000000000000000.xlog") {
			body, err := format.DecodeDMLBody(row.BodyRaw)
			if err != nil {
				continue
			}

			switch row.Type {
			case iproto.IPROTO_UPDATE:
				// The operations of an UPDATE are its IPROTO_TUPLE.
				require.Nil(t, body.Ops, version)
				update, err = format.DecodeOps(body.Tuple)
				require.NoError(t, err, version)
				// box.space:update numbers fields from 1.
				assert.Equal(t, []byte{0x01}, body.Extras[uint64(iproto.IPROTO_INDEX_BASE)], version)
			case iproto.IPROTO_UPSERT:
				upsert, err = format.DecodeOps(body.Ops)
				require.NoError(t, err, version)
			}
		}

		assert.Equal(t, []format.Operation{{Op: format.OpSet, Field: 2, Value: opS("alpha2")}}, update, version)
		assert.Equal(t, []format.Operation{{Op: format.OpSet, Field: 2, Value: opS("gamma2")}}, upsert, version)
	}

	// A vinyl UPSERT holds an array of operation lists, one per squashed
	// UPSERT, numbering fields from 0.
	var lists [][]format.Operation

	for _, row := range fixtureRows(t, "vinyl/701/0/00000000000000000014.run") {
		if row.Type != iproto.IPROTO_UPSERT {
			continue
		}

		body, err := format.DecodeVyStmtBody(row.BodyRaw)
		require.NoError(t, err)

		var raw []msgpack.RawMessage
		require.NoError(t, msgpack.Unmarshal(body.Ops, &raw))

		for _, list := range raw {
			ops, err := format.DecodeOps(list)
			require.NoError(t, err)

			lists = append(lists, ops)
		}
	}

	assert.Equal(t, [][]format.Operation{
		{{Op: format.OpAdd, Field: 1, Value: []byte{1}}},
		{{Op: format.OpAdd, Field: 1, Value: []byte{10}}, {Op: format.OpSet, Field: 2, Value: opS("x")}},
		{{Op: format.OpAdd, Field: 1, Value: []byte{1}}},
	}, lists)
}

// FuzzDecodeOps checks DecodeOps never panics and DecodeOpsInto, reusing one
// dst across inputs, agrees with it.
func FuzzDecodeOps(f *testing.F) {
	f.Add(opsBench())
	f.Add(opArr(opArr(opS("="), opStrOrBin("f", 32, true), opArr([]byte{1}))))
	f.Add([]byte{0x91, 0x95, 0xa1, ':', 0xd3, 0, 0, 0, 0, 0, 0, 0, 1, 0xff, 0x00, 0xa0})
	f.Add([]byte{0xdd, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{})

	var dst []format.Operation

	f.Fuzz(func(t *testing.T, b []byte) {
		want, werr := format.DecodeOps(b)

		var gerr error

		dst, gerr = format.DecodeOpsInto(b, dst)

		require.Equal(t, werr != nil, gerr != nil, "error disagreement")

		if werr != nil {
			require.Empty(t, dst)

			return
		}

		require.Len(t, dst, len(want))

		for i := range want {
			require.Equal(t, want[i], dst[i], "op %d", i)
		}
	})
}
