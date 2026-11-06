package format_test

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// mustVClockArray converts v, failing the test on an out-of-range id.
func mustVClockArray(t *testing.T, v format.VClock) format.VClockArray {
	t.Helper()

	a, err := format.VClockArrayFrom(v)
	require.NoError(t, err)

	return a
}

// encodeVClockMap encodes v as a msgpack {id: lsn} map, ids ascending, with
// a fixmap header up to 15 entries and a map16 header above.
func encodeVClockMap(v format.VClock) []byte {
	var out []byte

	if len(v) <= 15 {
		out = mpFixmap(len(v))
	} else {
		out = []byte{0xde, 0, 0}
		binary.BigEndian.PutUint16(out[1:], uint16(len(v)))
	}

	ids := make([]uint32, 0, len(v))
	for id := range v {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	for _, id := range ids {
		out = append(out, kv(uint64(id), mpUint(uint64(v[id])))...)
	}

	return out
}

// fullVClock sets every replica id 0..VClockMax-1.
func fullVClock() format.VClock {
	v := format.VClock{}
	for id := range uint32(format.VClockMax) {
		v[id] = int64(id)*1000 + 1
	}

	return v
}

func TestVClockArray_Set(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id      uint32
		wantErr bool
	}{
		{0, false},
		{1, false},
		{format.VClockMax - 1, false},
		{format.VClockMax, true},
		{format.VClockMax + 1, true},
		{math.MaxUint32, true},
	}
	for _, c := range cases {
		var a format.VClockArray

		err := a.Set(c.id, 42)
		if c.wantErr {
			require.ErrorIsf(t, err, format.ErrVClockReplicaID, "Set(%d)", c.id)
			assert.Truef(t, a.IsZero(), "Set(%d) failed but changed the value", c.id)
			assert.Falsef(t, a.Has(c.id), "Has(%d)", c.id)

			lsn, ok := a.Get(c.id)
			assert.Falsef(t, ok, "Get(%d) ok", c.id)
			assert.Zerof(t, lsn, "Get(%d) lsn", c.id)

			continue
		}

		require.NoErrorf(t, err, "Set(%d)", c.id)
		assert.Truef(t, a.Has(c.id), "Has(%d)", c.id)
		assert.Equalf(t, 1, a.Len(), "Len after Set(%d)", c.id)

		lsn, ok := a.Get(c.id)
		assert.Truef(t, ok, "Get(%d) ok", c.id)
		assert.Equalf(t, int64(42), lsn, "Get(%d) lsn", c.id)
	}
}

func TestVClockArray_SetZeroLSNIsPresent(t *testing.T) {
	t.Parallel()

	var a format.VClockArray

	require.NoError(t, a.Set(0, 0))
	assert.True(t, a.Has(0))
	assert.False(t, a.IsZero())
	assert.Equal(t, 1, a.Len())
	assert.Equal(t, "{0: 0}", a.String())

	a.Reset()
	assert.True(t, a.IsZero())
	assert.Equal(t, format.VClockArray{}, a)
}

func TestVClockArray_Converters(t *testing.T) {
	t.Parallel()

	cases := []format.VClock{
		{},
		{0: 0},
		{1: 42},
		{0: 0, 1: 42, 2: 7},
		{31: math.MaxInt64, 0: math.MinInt64},
		fullVClock(),
	}
	for _, want := range cases {
		a := mustVClockArray(t, want)
		assert.Equalf(t, len(want), a.Len(), "Len of %v", want)

		got := a.VClock()
		require.NotNil(t, got, "VClock() must not be nil")
		assert.Truef(t, equalVClock(got, want), "round-trip: got %v want %v", got, want)
	}

	a, err := format.VClockArrayFrom(nil)
	require.NoError(t, err)
	assert.True(t, a.IsZero(), "nil VClock gives the zero VClockArray")

	_, err = format.VClockArrayFrom(format.VClock{1: 1, format.VClockMax: 1})
	require.ErrorIs(t, err, format.ErrVClockReplicaID)
}

func TestVClockArray_Equality(t *testing.T) {
	t.Parallel()

	var a, b format.VClockArray

	require.NoError(t, a.Set(3, 7))
	require.NoError(t, b.Set(3, 7))
	require.Equal(t, a, b, "same content must compare ==")

	require.NoError(t, b.Set(4, 0))
	require.NotEqual(t, a, b, "an extra id at LSN 0 must compare !=")
}

func TestVClockArray_SignatureMatchesVClock(t *testing.T) {
	t.Parallel()

	cases := []format.VClock{
		{},
		{1: 42, 2: 7, 3: 0},
		{0: -5, 31: 10},
		fullVClock(),
	}
	for _, v := range cases {
		a := mustVClockArray(t, v)
		assert.Equalf(t, v.Signature(), a.Signature(), "Signature of %v", v)
	}
}

func TestVClockArray_CompareMatchesVClock(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b   format.VClock
		order  int
		canCmp bool
	}{
		{format.VClock{}, format.VClock{}, 0, true},
		{format.VClock{1: 10}, format.VClock{1: 10}, 0, true},
		{format.VClock{1: 5}, format.VClock{1: 10}, -1, true},
		{format.VClock{1: 10}, format.VClock{1: 5}, 1, true},
		{format.VClock{1: 5, 2: 3}, format.VClock{1: 3, 2: 5}, 0, false},
		{format.VClock{1: 5, 2: 3}, format.VClock{1: 5}, 1, true},
		{format.VClock{1: 5}, format.VClock{1: 5, 2: 3}, -1, true},
		{format.VClock{1: 0}, format.VClock{}, 0, true}, // A set id at LSN 0 equals an unset one.
		{format.VClock{0: 1, 31: 2}, format.VClock{0: 1, 31: 3}, -1, true},
		{format.VClock{0: 2, 31: 2}, format.VClock{0: 1, 31: 3}, 0, false},
		{fullVClock(), fullVClock(), 0, true},
		{fullVClock(), format.VClock{}, 1, true},
	}
	for i, c := range cases {
		mapOrder, mapOK := c.a.Compare(c.b)
		require.Equalf(t, c.canCmp, mapOK, "case %d: VClock.Compare ok", i)

		if mapOK {
			require.Equalf(t, c.order, mapOrder, "case %d: VClock.Compare order", i)
		}

		a, b := mustVClockArray(t, c.a), mustVClockArray(t, c.b)
		order, ok := a.Compare(&b)
		assert.Equalf(t, mapOK, ok, "case %d (%v vs %v): ok", i, c.a, c.b)
		assert.Equalf(t, mapOrder, order, "case %d (%v vs %v): order", i, c.a, c.b)
	}
}

func TestVClockArray_StringMatchesVClock(t *testing.T) {
	t.Parallel()

	cases := []format.VClock{
		{},
		{0: 0},
		{1: 42},
		{3: 30, 1: 10, 2: 20},
		{0: math.MinInt64, 31: math.MaxInt64},
		fullVClock(),
	}
	for _, v := range cases {
		a := mustVClockArray(t, v)
		assert.Equal(t, v.String(), a.String())

		back, err := format.ParseVClock(a.String())
		require.NoError(t, err)
		assert.Truef(t, equalVClock(back, v), "parse %q: got %v want %v", a.String(), back, v)
	}
}

// TestDecodeVClockArrayInto_MatchesVClockDecoder checks the decoder against
// the VClock map decoder, reached through DecodeRaftBody's
// IPROTO_RAFT_VCLOCK.
func TestDecodeVClockArrayInto_MatchesVClockDecoder(t *testing.T) {
	t.Parallel()

	cases := []format.VClock{
		{},
		{0: 0},
		{1: 100, 2: 200},
		{0: 1, 7: math.MaxUint32 + 1, 31: math.MaxInt64},
		fullVClock(), // A map16 header.
	}
	for _, v := range cases {
		enc := encodeVClockMap(v)

		rb, err := format.DecodeRaftBody(buildMap(1, kv(uint64(iproto.IPROTO_RAFT_VCLOCK), enc)))
		require.NoError(t, err)

		var dst format.VClockArray

		// Pre-fill dst so a missing reset shows up.
		require.NoError(t, dst.Set(5, 555))

		n, err := format.DecodeVClockArrayInto(append(slices.Clone(enc), 0xc0), &dst)
		require.NoErrorf(t, err, "decode %v", v)
		assert.Equalf(t, len(enc), n, "consumed bytes of %v", v)
		assert.Truef(t, equalVClock(dst.VClock(), rb.VClock), "decode: got %v want %v", dst.VClock(), rb.VClock)
	}
}

func TestDecodeVClockArrayInto_DuplicateIDKeepsLast(t *testing.T) {
	t.Parallel()

	enc := buildMap(2, append(kv(1, mpUint(10)), kv(1, mpUint(20))...))

	rb, err := format.DecodeRaftBody(buildMap(1, kv(uint64(iproto.IPROTO_RAFT_VCLOCK), enc)))
	require.NoError(t, err)

	var dst format.VClockArray

	_, err = format.DecodeVClockArrayInto(enc, &dst)
	require.NoError(t, err)
	assert.Truef(t, equalVClock(dst.VClock(), rb.VClock), "got %v want %v", dst.VClock(), rb.VClock)
	assert.Equal(t, 1, dst.Len())
}

func TestDecodeVClockArrayInto_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		b    []byte
		is   error
	}{
		{"no input", nil, format.ErrEmptyInput},
		{"nil tag", []byte{0xc0}, format.ErrUnexpectedTag},
		{"map16 short", []byte{0xde, 0x00}, format.ErrShortInput},
		{"missing id", mpFixmap(1), format.ErrEmptyInput},
		{"missing lsn", append(mpFixmap(1), mpUint(1)...), format.ErrEmptyInput},
		{"string id", append(mpFixmap(1), 0xa1, 'x', 0x01), format.ErrUnexpectedTag},
		{"id out of range", buildMap(1, kv(format.VClockMax, mpUint(1))), format.ErrVClockReplicaID},
		{"huge id", buildMap(1, kv(math.MaxUint64, mpUint(1))), format.ErrVClockReplicaID},
		{"after a good entry", buildMap(2, append(kv(1, mpUint(1)), kv(40, mpUint(1))...)), format.ErrVClockReplicaID},
	}
	for _, c := range cases {
		var dst format.VClockArray

		require.NoError(t, dst.Set(2, 2))

		n, err := format.DecodeVClockArrayInto(c.b, &dst)
		require.ErrorIsf(t, err, c.is, "%s", c.name)
		assert.Zerof(t, n, "%s: consumed", c.name)
		assert.Truef(t, dst.IsZero(), "%s: dst must be left reset, got %v", c.name, &dst)
	}
}

// TestVClockArray_ZeroAlloc is not parallel: AllocsPerRun counts the mallocs
// of the whole process, so a test running alongside would inflate them.
func TestVClockArray_ZeroAlloc(t *testing.T) { //nolint:paralleltest // AllocsPerRun counts process-wide mallocs
	if testing.Short() {
		t.Skip("allocation counts: skipped in -short mode")
	}

	enc := encodeVClockMap(fullVClock())

	var dst format.VClockArray

	decodeAllocs := testing.AllocsPerRun(100, func() {
		if _, err := format.DecodeVClockArrayInto(enc, &dst); err != nil {
			panic(err)
		}
	})
	assert.Zero(t, decodeAllocs, "DecodeVClockArrayInto allocs")

	a := mustVClockArray(t, fullVClock())
	b := mustVClockArray(t, format.VClock{1: 1})

	var sink int64

	sigAllocs := testing.AllocsPerRun(100, func() {
		sink += a.Signature()
	})
	assert.Zero(t, sigAllocs, "Signature allocs")

	cmpAllocs := testing.AllocsPerRun(100, func() {
		order, _ := a.Compare(&b)
		sink += int64(order)
	})
	assert.Zero(t, cmpAllocs, "Compare allocs")

	setAllocs := testing.AllocsPerRun(100, func() {
		_ = b.Set(3, sink)
		lsn, _ := b.Get(3)
		sink += lsn
	})
	assert.Zero(t, setAllocs, "Set/Get allocs")
}

func BenchmarkDecodeVClockArrayInto(b *testing.B) {
	enc := encodeVClockMap(format.VClock{1: 100, 2: 200, 3: 300})

	var dst format.VClockArray

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := format.DecodeVClockArrayInto(enc, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeVClockMap is the baseline: the same map through the VClock
// decoder, reached via DecodeRaftBody.
func BenchmarkDecodeVClockMap(b *testing.B) {
	body := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_VCLOCK), encodeVClockMap(format.VClock{1: 100, 2: 200, 3: 300})))

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := format.DecodeRaftBody(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVClockArray_Compare(b *testing.B) {
	x, err := format.VClockArrayFrom(format.VClock{1: 100, 2: 200, 3: 300})
	if err != nil {
		b.Fatal(err)
	}

	y, err := format.VClockArrayFrom(format.VClock{1: 100, 2: 201, 3: 300})
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if order, ok := x.Compare(&y); !ok || order != -1 {
			b.Fatal(order, ok)
		}
	}
}

func BenchmarkVClock_Compare(b *testing.B) {
	x := format.VClock{1: 100, 2: 200, 3: 300}
	y := format.VClock{1: 100, 2: 201, 3: 300}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if order, ok := x.Compare(y); !ok || order != -1 {
			b.Fatal(order, ok)
		}
	}
}
