package memsize_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/memsize"
)

func TestPrimaryKeyCanonicalNumbers(t *testing.T) {
	t.Parallel()

	pk := &memsize.Index{Parts: []memsize.Part{{FieldNo: 0, Type: "number"}}}
	tests := []struct {
		name  string
		tuple []byte
		want  []byte
	}{
		{name: "positive fixint", tuple: []byte{0x91, 0x01}, want: []byte{0x91, 0x01}},
		{name: "uint8", tuple: []byte{0x91, 0xcc, 0x01}, want: []byte{0x91, 0x01}},
		{name: "int64", tuple: []byte{0x91, 0xd3, 0, 0, 0, 0, 0, 0, 0, 1}, want: []byte{0x91, 0x01}},
		{name: "integral double", tuple: []byte{0x91, 0xcb, 0x3f, 0xf0, 0, 0, 0, 0, 0, 0}, want: []byte{0x91, 0x01}},
		{name: "negative int16", tuple: []byte{0x91, 0xd1, 0xff, 0xff}, want: []byte{0x91, 0xff}},
		{
			name:  "negative int32",
			tuple: []byte{0x91, 0xd2, 0xff, 0xff, 0x7f, 0xff},
			want:  []byte{0x91, 0xd2, 0xff, 0xff, 0x7f, 0xff},
		},
		{
			name:  "nonintegral float32",
			tuple: []byte{0x91, 0xca, 0x3f, 0xc0, 0, 0},
			want:  []byte{0x91, 0xcb, 0x3f, 0xf8, 0, 0, 0, 0, 0, 0},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			key, err := memsize.PrimaryKeyBytes(test.tuple, pk)
			require.NoError(t, err)
			assert.Equal(t, test.want, key)
		})
	}
}

func TestPrimaryKeyUsesPartOrder(t *testing.T) {
	t.Parallel()

	pk := &memsize.Index{Parts: []memsize.Part{
		{FieldNo: 2, Type: "string"},
		{FieldNo: 0, Type: "unsigned"},
	}}
	tuple := []byte{0x93, 0xcc, 0x07, 0xc3, 0xd9, 0x01, 'x'}

	key, err := memsize.PrimaryKeyBytes(tuple, pk)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x92, 0xa1, 'x', 0x07}, key)

	hash, err := memsize.PrimaryKey(tuple, pk)
	require.NoError(t, err)
	assert.NotZero(t, hash)
}

func TestPrimaryKeyRejectsUnsupportedSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pk   *memsize.Index
		want error
	}{
		{
			name: "collation",
			pk:   &memsize.Index{Parts: []memsize.Part{{FieldNo: 0, Type: "string", Collation: "unicode_ci"}}},
			want: memsize.ErrUnsupportedPKCollation,
		},
		{
			name: "decimal",
			pk:   &memsize.Index{Parts: []memsize.Part{{FieldNo: 0, Type: "decimal"}}},
			want: memsize.ErrUnsupportedPKType,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := memsize.PrimaryKeyBytes([]byte{0x91, 0x01}, test.pk)
			require.Error(t, err)
			assert.ErrorIs(t, err, test.want)
		})
	}
}
