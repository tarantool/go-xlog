package format_test

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
)

func TestMPCursorRoundTrip(t *testing.T) {
	t.Parallel()

	wire := []byte{
		0x98,             // array(8)
		0xcd, 0x01, 0x2c, // uint16(300)
		0xd1, 0xff, 0x7f, // int16(-129)
		0xa3, 'f', 'o', 'o', // fixstr("foo")
		0xc3,                  // true
		0x81, 0xa1, 'k', 0x2a, // map(1): {"k": 42}
		0x92, 0x01, 0x02, // array(2), returned raw
		0x91, 0xa1, 'x', // array(1), skipped
		0xc2, // false
	}

	cursor := format.NewMPCursor(wire)

	n, err := cursor.ArrayLen()
	require.NoError(t, err)
	assert.Equal(t, 8, n)

	u, err := cursor.Uint()
	require.NoError(t, err)
	assert.Equal(t, uint64(300), u)

	i, err := cursor.Int()
	require.NoError(t, err)
	assert.Equal(t, int64(-129), i)

	str, err := cursor.Str()
	require.NoError(t, err)
	assert.Equal(t, []byte("foo"), str)
	str[0] = 'F'

	assert.Equal(t, byte('F'), wire[8], "Str must alias the input")

	b, err := cursor.Bool()
	require.NoError(t, err)
	assert.True(t, b)

	n, err = cursor.MapLen()
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	key, err := cursor.Str()
	require.NoError(t, err)
	assert.Equal(t, []byte("k"), key)

	u, err = cursor.Uint()
	require.NoError(t, err)
	assert.Equal(t, uint64(42), u)

	raw, err := cursor.Raw()
	require.NoError(t, err)
	assert.Equal(t, []byte{0x92, 0x01, 0x02}, raw)
	raw[1] = 0x03

	assert.Equal(t, byte(0x03), wire[17], "Raw must alias the input")

	require.NoError(t, cursor.Skip())

	b, err = cursor.Bool()
	require.NoError(t, err)
	assert.False(t, b)
	assert.False(t, cursor.More())
}

func TestMPCursorIntWidths(t *testing.T) {
	t.Parallel()

	uint64MaxInt := []byte{0xcf, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint64(uint64MaxInt[1:], math.MaxInt64)

	int64Min := []byte{0xd3, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint64(int64Min[1:], uint64(1)<<63)

	tests := []struct {
		name string
		wire []byte
		want int64
	}{
		{name: "positive fixint", wire: []byte{0x7f}, want: 127},
		{name: "negative fixint", wire: []byte{0xff}, want: -1},
		{name: "uint8", wire: []byte{0xcc, 0x80}, want: 128},
		{name: "uint16", wire: []byte{0xcd, 0x01, 0x00}, want: 256},
		{name: "uint32", wire: []byte{0xce, 0, 1, 0, 0}, want: 65536},
		{name: "uint64", wire: uint64MaxInt, want: math.MaxInt64},
		{name: "int8", wire: []byte{0xd0, 0xdf}, want: -33},
		{name: "int16", wire: []byte{0xd1, 0xff, 0x7f}, want: -129},
		{name: "int32", wire: []byte{0xd2, 0xff, 0xff, 0x7f, 0xff}, want: -32769},
		{name: "int64", wire: int64Min, want: math.MinInt64},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cursor := format.NewMPCursor(test.wire)
			got, err := cursor.Int()
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
			assert.False(t, cursor.More())
		})
	}
}

func TestMPCursorContainerAndStringWidths(t *testing.T) {
	t.Parallel()

	str8 := append([]byte{0xd9, 3}, "one"...)
	str16 := append([]byte{0xda, 0, 3}, "two"...)
	str32 := append([]byte{0xdb, 0, 0, 0, 5}, "three"...)
	wire := append([]byte{0xdc, 0, 0, 0xdd, 0, 0, 0, 0, 0xde, 0, 0, 0xdf, 0, 0, 0, 0}, str8...)
	wire = append(wire, str16...)
	wire = append(wire, str32...)

	cursor := format.NewMPCursor(wire)
	array16, err := cursor.ArrayLen()
	require.NoError(t, err)
	assert.Zero(t, array16)

	array32, err := cursor.ArrayLen()
	require.NoError(t, err)
	assert.Zero(t, array32)

	map16, err := cursor.MapLen()
	require.NoError(t, err)
	assert.Zero(t, map16)

	map32, err := cursor.MapLen()
	require.NoError(t, err)
	assert.Zero(t, map32)

	for _, want := range []string{"one", "two", "three"} {
		got, err := cursor.Str()
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	}

	assert.False(t, cursor.More())
}

func TestMPCursorErrorsDoNotAdvance(t *testing.T) {
	t.Parallel()

	t.Run("uint64 overflows int64", func(t *testing.T) {
		t.Parallel()

		cursor := format.NewMPCursor([]byte{0xcf, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		_, err := cursor.Int()
		require.ErrorIs(t, err, format.ErrIntegerOverflow)
		assert.True(t, cursor.More())
	})

	t.Run("wrong scalar type", func(t *testing.T) {
		t.Parallel()

		cursor := format.NewMPCursor([]byte{0xc0})
		_, err := cursor.Bool()
		require.ErrorIs(t, err, format.ErrUnexpectedTag)
		assert.True(t, cursor.More())
	})

	t.Run("truncated raw value", func(t *testing.T) {
		t.Parallel()

		cursor := format.NewMPCursor([]byte{0x92, 0x01})
		_, err := cursor.Raw()
		require.ErrorIs(t, err, format.ErrTruncatedInput)
		assert.True(t, cursor.More())
	})
}

func TestMPCursorNoAlloc(t *testing.T) { //nolint:paralleltest // testing.AllocsPerRun rejects parallel tests.
	wire := []byte{0x94, 0x2a, 0xff, 0xa1, 'x', 0xc3}
	allocs := testing.AllocsPerRun(1000, func() {
		cursor := format.NewMPCursor(wire)
		_, _ = cursor.ArrayLen()
		_, _ = cursor.Uint()
		_, _ = cursor.Int()
		_, _ = cursor.Str()
		_, _ = cursor.Bool()
	})

	assert.Zero(t, allocs)
}
