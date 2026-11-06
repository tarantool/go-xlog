package format_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// cloneDMLRow encodes an INSERT row whose DML body holds the tuple [n, "x"],
// x being the byte c: rows of different n and c have the same size and
// layout, as two rows a wire consumer receives one after the other into one
// buffer.
func cloneDMLRow(t *testing.T, n, c byte) []byte {
	t.Helper()

	body := make([]byte, 0, 16)
	body = append(body, kv(uint64(iproto.IPROTO_SPACE_ID), mpUint(512))...)
	body = append(body, kv(uint64(iproto.IPROTO_TUPLE), []byte{0x92, n, 0xa1, c})...)

	enc, err := format.EncodeXRow(nil, &format.XRow{
		Type: iproto.IPROTO_INSERT, LSN: int64(n), TSN: int64(n),
		Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: buildMap(2, body),
	})
	require.NoError(t, err)

	return enc
}

// decodeDMLRow decodes the row at b and its DML body.
func decodeDMLRow(t *testing.T, b []byte, row *format.XRow, body *format.DMLBody) {
	t.Helper()

	_, err := format.DecodeXRowInto(b, row)
	require.NoError(t, err)
	require.NoError(t, format.DecodeDMLBodyInto(row.BodyRaw, body, nil))
}

// TestAliasing_NextRowOverwritesRetained pins that the aliasing is the
// contract, not an accident: a row and a body decoded from a buffer change
// when the buffer does. A wire consumer that receives the next row into the
// same buffer reads it through what it kept of the last one.
func TestAliasing_NextRowOverwritesRetained(t *testing.T) {
	t.Parallel()

	first, second := cloneDMLRow(t, 1, 'a'), cloneDMLRow(t, 2, 'b')
	require.Len(t, second, len(first), "the rows must have the same layout")

	buf := append([]byte(nil), first...)

	var (
		row  format.XRow
		body format.DMLBody
	)

	decodeDMLRow(t, buf, &row, &body)
	require.Equal(t, []byte{0x92, 0x01, 0xa1, 'a'}, body.Tuple)

	keptBody := append([]byte(nil), row.BodyRaw...)

	copy(buf, second) // The next packet, received into the same buffer.

	assert.NotEqual(t, keptBody, row.BodyRaw, "BodyRaw aliases the buffer")
	assert.Equal(t, []byte{0x92, 0x02, 0xa1, 'b'}, body.Tuple, "Tuple aliases the buffer: it reads the next row")
}

// TestClone_SurvivesBufferReuse - what Clone and CloneInto return does not
// change with the buffer it was decoded from.
func TestClone_SurvivesBufferReuse(t *testing.T) {
	t.Parallel()

	first, second := cloneDMLRow(t, 1, 'a'), cloneDMLRow(t, 2, 'b')
	buf := append([]byte(nil), first...)

	var (
		row        format.XRow
		body       format.DMLBody
		intoRow    format.XRow
		intoBody   format.DMLBody
		wantRow    format.XRow
		wantBodyEq format.DMLBody
	)

	decodeDMLRow(t, first, &wantRow, &wantBodyEq)
	decodeDMLRow(t, buf, &row, &body)

	clonedRow, clonedBody := row.Clone(), body.Clone()
	row.CloneInto(&intoRow)
	body.CloneInto(&intoBody)

	copy(buf, second)

	require.NotEqual(t, wantBodyEq, body, "the decoded body must have changed for the test to mean anything")
	assert.Equal(t, wantRow, *clonedRow, "Clone of the row")
	assert.Equal(t, wantBodyEq, *clonedBody, "Clone of the body")
	assert.Equal(t, wantRow, intoRow, "CloneInto of the row")
	assert.Equal(t, wantBodyEq, intoBody, "CloneInto of the body")
}

// TestCloneInto_DstFromSameBuffer - CloneInto into a dst decoded from the
// buffer the source was decoded from does not write into that buffer, and
// leaves dst owning its bytes.
func TestCloneInto_DstFromSameBuffer(t *testing.T) {
	t.Parallel()

	first, second := cloneDMLRow(t, 1, 'a'), cloneDMLRow(t, 2, 'b')
	buf := append(append([]byte(nil), first...), second...)
	orig := append([]byte(nil), buf...)

	var (
		kept, row         format.XRow
		keptBody, rowBody format.DMLBody
	)

	decodeDMLRow(t, buf, &kept, &keptBody)
	decodeDMLRow(t, buf[len(first):], &row, &rowBody)

	want, wantBody := row.Clone(), rowBody.Clone()

	row.CloneInto(&kept)
	rowBody.CloneInto(&keptBody)
	require.Equal(t, orig, buf, "CloneInto must not write into the buffer dst aliased")

	for i := range buf {
		buf[i] = 0xee
	}

	assert.Equal(t, *want, kept, "the row CloneInto left in dst")
	assert.Equal(t, *wantBody, keptBody, "the body CloneInto left in dst")
}

// TestCloneInto_Self - CloneInto of a value into itself, or into a shallow
// copy of it, detaches it from the buffer.
func TestCloneInto_Self(t *testing.T) {
	t.Parallel()

	buf := cloneDMLRow(t, 1, 'a')

	var (
		row  format.XRow
		body format.DMLBody
	)

	decodeDMLRow(t, buf, &row, &body)

	want, wantBody := row.Clone(), body.Clone()
	shallow, shallowBody := row, body

	row.CloneInto(&shallow)
	body.CloneInto(&shallowBody)
	row.CloneInto(&row)
	body.CloneInto(&body)

	for i := range buf {
		buf[i] = 0xee
	}

	assert.Equal(t, *want, row, "row cloned into itself")
	assert.Equal(t, *wantBody, body, "body cloned into itself")
	assert.Equal(t, *want, shallow, "row cloned into a shallow copy")
	assert.Equal(t, *wantBody, shallowBody, "body cloned into a shallow copy")
}

// fillMode is how fill sets the slices and maps of a value.
type fillMode int

const (
	fillRich  fillMode = iota // Non-empty, of a size growing with the seed.
	fillEmpty                 // Empty, but not nil.
)

// fill sets every field of v by reflection: numbers to seed, bytes and maps
// per mode. It fails on a kind it does not know, so a field of a new kind
// cannot be added to a cloned type without the test learning it.
func fill(t *testing.T, v reflect.Value, seed int, mode fillMode) {
	t.Helper()

	switch v.Kind() { //nolint:exhaustive // Every other kind fails below.
	case reflect.Struct:
		for i := range v.NumField() {
			fill(t, v.Field(i), seed+i, mode)
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		v.SetInt(int64(seed + 1))
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		v.SetUint(uint64(seed + 1))
	case reflect.Float64:
		v.SetFloat(float64(seed) + 0.5)
	case reflect.Slice:
		require.Equalf(t, reflect.Uint8, v.Type().Elem().Kind(), "fill: a slice of %s: teach fill and Clone", v.Type())

		b := []byte{}

		if mode == fillRich {
			for i := range seed + 3 {
				b = append(b, byte(seed+i))
			}
		}

		v.SetBytes(b)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())

		if mode == fillRich {
			for i := range 2 {
				k := reflect.New(v.Type().Key()).Elem()
				e := reflect.New(v.Type().Elem()).Elem()

				fill(t, k, seed+i, mode)
				fill(t, e, seed+i, mode)
				m.SetMapIndex(k, e)
			}
		}

		v.Set(m)
	default:
		t.Fatalf("fill: a field of kind %s (%s): teach fill and Clone", v.Kind(), v.Type())
	}
}

// filled returns a new T with every field set.
func filled[T any](t *testing.T, seed int, mode fillMode) *T {
	t.Helper()

	v := new(T)
	fill(t, reflect.ValueOf(v).Elem(), seed, mode)

	return v
}

// sharesNoMemory fails if a slice or a map of got shares memory with the
// same one of src.
func sharesNoMemory(t *testing.T, path string, got, src reflect.Value) {
	t.Helper()

	switch got.Kind() { //nolint:exhaustive // Only structs, slices and maps hold memory.
	case reflect.Struct:
		for i := range got.NumField() {
			sharesNoMemory(t, path+"."+got.Type().Field(i).Name, got.Field(i), src.Field(i))
		}
	case reflect.Slice:
		if got.Len() > 0 && src.Len() > 0 {
			assert.NotEqualf(t, src.Pointer(), got.Pointer(), "%s shares the array of the source", path)
		}
	case reflect.Map:
		if got.IsNil() || src.IsNil() {
			return
		}

		assert.NotEqualf(t, src.Pointer(), got.Pointer(), "%s is the map of the source", path)

		for _, k := range src.MapKeys() {
			if g := got.MapIndex(k); g.IsValid() {
				sharesNoMemory(t, path+"["+k.String()+"]", g, src.MapIndex(k))
			}
		}
	}
}

// checkCloneDeep checks clone and cloneInto, the Clone and CloneInto of T,
// on a T with every field set (rich), with every slice and map empty but
// non-nil, and with none (the zero value): each returns a value equal to
// the source - nil stays nil and empty stays empty - that shares no memory
// with it. cloneInto is checked into a zero dst, into a dst holding a bigger
// value, into one holding a smaller value, into an empty one, into a shallow
// copy of the source and into the source itself.
func checkCloneDeep[T any](t *testing.T, clone func(*T) *T, cloneInto func(src, dst *T)) {
	t.Helper()

	srcs := map[string]*T{
		"rich":  filled[T](t, 1, fillRich),
		"empty": filled[T](t, 1, fillEmpty),
		"zero":  new(T),
	}

	for name, src := range srcs {
		// The source as it was: the cloneInto into itself below replaces
		// its slices.
		orig := *src

		check := func(what string, got *T) {
			t.Helper()

			assert.Equalf(t, orig, *got, "%s: %s", name, what)
			sharesNoMemory(t, reflect.TypeOf(orig).Name(), reflect.ValueOf(got).Elem(), reflect.ValueOf(orig))
		}

		check("Clone", clone(src))

		for dstName, dst := range map[string]*T{
			"zero":    new(T),
			"bigger":  filled[T](t, 7, fillRich),
			"smaller": filled[T](t, 0, fillRich),
			"empty":   filled[T](t, 0, fillEmpty),
		} {
			cloneInto(src, dst)
			check("CloneInto a "+dstName+" dst", dst)
		}

		shallow := *src
		cloneInto(src, &shallow)
		check("CloneInto a shallow copy", &shallow)

		cloneInto(src, src)
		check("CloneInto itself", src)
	}
}

// TestClone_DeepAndEqual runs checkCloneDeep on every type with a Clone.
// Every field of a cloned type is filled by reflection, so a []byte or a map
// field added to one without being copied by its Clone fails here.
func TestClone_DeepAndEqual(t *testing.T) {
	t.Parallel()

	t.Run("XRow", func(t *testing.T) {
		t.Parallel()
		checkCloneDeep(t, (*format.XRow).Clone, (*format.XRow).CloneInto)
	})
	t.Run("DMLBody", func(t *testing.T) {
		t.Parallel()
		checkCloneDeep(t, (*format.DMLBody).Clone, (*format.DMLBody).CloneInto)
	})
	t.Run("RaftBody", func(t *testing.T) {
		t.Parallel()
		checkCloneDeep(t, (*format.RaftBody).Clone, (*format.RaftBody).CloneInto)
	})
	t.Run("SynchroBody", func(t *testing.T) {
		t.Parallel()
		checkCloneDeep(t, (*format.SynchroBody).Clone, (*format.SynchroBody).CloneInto)
	})
	t.Run("VyLogBody", func(t *testing.T) {
		t.Parallel()
		checkCloneDeep(t, (*format.VyLogBody).Clone, (*format.VyLogBody).CloneInto)
	})
	t.Run("VyStmtBody", func(t *testing.T) {
		t.Parallel()
		checkCloneDeep(t, (*format.VyStmtBody).Clone, (*format.VyStmtBody).CloneInto)
	})
	t.Run("VyRunInfo", func(t *testing.T) {
		t.Parallel()
		checkCloneDeep(t, (*format.VyRunInfo).Clone, (*format.VyRunInfo).CloneInto)
	})
	t.Run("VyPageInfo", func(t *testing.T) {
		t.Parallel()
		checkCloneDeep(t, (*format.VyPageInfo).Clone, (*format.VyPageInfo).CloneInto)
	})
	t.Run("Operation", func(t *testing.T) {
		t.Parallel()

		clone := func(op *format.Operation) *format.Operation {
			c := op.Clone()

			return &c
		}
		checkCloneDeep(t, clone, (*format.Operation).CloneInto)
	})
}

// TestClone_NilReceiver - Clone of a nil pointer is nil, as for Meta.
func TestClone_NilReceiver(t *testing.T) {
	t.Parallel()

	assert.Nil(t, (*format.XRow)(nil).Clone())
	assert.Nil(t, (*format.DMLBody)(nil).Clone())
	assert.Nil(t, (*format.RaftBody)(nil).Clone())
	assert.Nil(t, (*format.SynchroBody)(nil).Clone())
	assert.Nil(t, (*format.VyLogBody)(nil).Clone())
	assert.Nil(t, (*format.VyStmtBody)(nil).Clone())
	assert.Nil(t, (*format.VyRunInfo)(nil).Clone())
	assert.Nil(t, (*format.VyPageInfo)(nil).Clone())
}

// TestClone_DecodedBodies - the clone of every decoded body equals it and
// survives its buffer.
func TestClone_DecodedBodies(t *testing.T) {
	t.Parallel()

	decoders := []struct {
		name   string
		body   []byte
		decode func([]byte) (any, error)
	}{
		{"DML", dmlRichBody(), func(b []byte) (any, error) { return format.DecodeDMLBody(b) }},
		{"RAFT", raftBody(true), func(b []byte) (any, error) { return format.DecodeRaftBody(b) }},
		{"PROMOTE", synchroPromoteBody(), func(b []byte) (any, error) { return format.DecodeSynchroBody(b) }},
		{"VYLOG", vyLogBody(format.VyLogCreateRun, kv(1, mpUint(3)), kv(2, []byte{0x91, 0x01})), func(b []byte) (any, error) { return format.DecodeVyLogBody(b) }},
		{"run statement", vyStmtRichBody(), func(b []byte) (any, error) { return format.DecodeVyStmtBody(b) }},
		{"run info", runInfo(kv(9, []byte{0xc4, 0x01, 0xff})), func(b []byte) (any, error) { return format.DecodeVyRunInfoBody(b) }},
		{"page info", pageInfoBody(), func(b []byte) (any, error) { return format.DecodeVyPageInfoBody(b) }},
	}

	for _, d := range decoders {
		buf := append([]byte(nil), d.body...)

		decoded, err := d.decode(buf)
		require.NoError(t, err, d.name)

		want, err := d.decode(d.body)
		require.NoError(t, err, d.name)

		clone := reflect.ValueOf(decoded).MethodByName("Clone").Call(nil)[0].Interface()

		for i := range buf {
			buf[i] = 0xee
		}

		assert.Equal(t, want, clone, d.name)
	}
}

// TestCloneOps - CloneOps and CloneOpsInto deep-copy operations, nil stays
// nil and empty stays empty, and CloneOpsInto reuses dst.
func TestCloneOps(t *testing.T) {
	t.Parallel()

	raw := []byte{
		0x92,
		0x93, 0xa1, '=', 0x01, 0xa1, 'x', // [=, 1, "x"]
		0x95, 0xa1, ':', 0xa3, 'f', 'o', 'o', 0x00, 0x01, 0xa1, 'y', // [:, "foo", 0, 1, "y"]
	}
	buf := append([]byte(nil), raw...)

	ops, err := format.DecodeOps(buf)
	require.NoError(t, err)

	want, err := format.DecodeOps(raw)
	require.NoError(t, err)

	clone := format.CloneOps(ops)
	into := format.CloneOpsInto(ops, nil)
	reused := format.CloneOpsInto(ops, make([]format.Operation, 3, 8))
	self := format.CloneOpsInto(ops, append([]format.Operation(nil), ops...))

	for i := range buf {
		buf[i] = 0xee
	}

	assert.Equal(t, want, clone, "CloneOps")
	assert.Equal(t, want, into, "CloneOpsInto nil")
	assert.Equal(t, want, reused, "CloneOpsInto a bigger dst")
	assert.Equal(t, 8, cap(reused), "CloneOpsInto reuses the capacity of dst")
	assert.Equal(t, want, self, "CloneOpsInto a copy of the source")

	assert.Nil(t, format.CloneOps(nil))
	assert.Nil(t, format.CloneOpsInto(nil, make([]format.Operation, 2)))
	assert.Equal(t, []format.Operation{}, format.CloneOps([]format.Operation{}))
	assert.Equal(t, []format.Operation{}, format.CloneOpsInto([]format.Operation{}, nil))
}

// TestCloneInto_ZeroAlloc is not parallel: AllocsPerRun counts the mallocs
// of the whole process. Once dst holds a value as big as the source,
// CloneInto copies into its slices and allocates nothing: a loop that keeps
// every row it reads in a slot it owns pays no allocation per row.
func TestCloneInto_ZeroAlloc(t *testing.T) { //nolint:paralleltest // AllocsPerRun counts process-wide mallocs
	if testing.Short() {
		t.Skip("allocation counts: skipped in -short mode")
	}

	// Two rows of the same size, cloned in turn into one dst.
	var (
		rows   [2]format.XRow
		bodies [2]format.DMLBody
		stmts  [2]format.VyStmtBody
		infos  [2]format.VyRunInfo
		pages  [2]format.VyPageInfo
		opses  [2][]format.Operation
	)

	for i, b := range [][]byte{cloneDMLRow(t, 1, 'a'), cloneDMLRow(t, 2, 'b')} {
		decodeDMLRow(t, b, &rows[i], &bodies[i])
		require.NoError(t, format.DecodeVyStmtBodyInto(vyStmtRichBody(), &stmts[i], nil))
		require.NoError(t, format.DecodeVyRunInfoBodyInto(runInfo(kv(9, []byte{0xc4, 0x01, byte(i)})), &infos[i], nil))
		require.NoError(t, format.DecodeVyPageInfoBodyInto(pageInfoBody(), &pages[i], nil))

		ops, err := format.DecodeOps([]byte{0x91, 0x93, 0xa1, '=', 0x01, 0xa1, 'a' + byte(i)})
		require.NoError(t, err)

		opses[i] = ops
	}

	var (
		row  format.XRow
		body format.DMLBody
		stmt format.VyStmtBody
		info format.VyRunInfo
		page format.VyPageInfo
		ops  []format.Operation
		n    int
	)

	cases := []struct {
		name string
		fn   func(i int)
	}{
		{"XRow", func(i int) { rows[i].CloneInto(&row) }},
		{"DMLBody", func(i int) { bodies[i].CloneInto(&body) }},
		{"VyStmtBody", func(i int) { stmts[i].CloneInto(&stmt) }},
		{"VyRunInfo", func(i int) { infos[i].CloneInto(&info) }},
		{"VyPageInfo", func(i int) { pages[i].CloneInto(&page) }},
		{"CloneOpsInto", func(i int) { ops = format.CloneOpsInto(opses[i], ops) }},
	}

	for _, c := range cases {
		fn := c.fn
		fn(0) // Warm dst up.

		allocs := testing.AllocsPerRun(100, func() {
			fn(n % 2)
			n++
		})
		assert.Zerof(t, allocs, "%s: allocs", c.name)
	}

	// Clone copies all the raw fields of a value into one allocation, next
	// to that of the value.
	var (
		rowSink  *format.XRow
		bodySink *format.DMLBody
	)

	assert.InDelta(t, 2, testing.AllocsPerRun(100, func() { rowSink = rows[0].Clone() }), 0, "XRow.Clone allocs")
	assert.InDelta(t, 2, testing.AllocsPerRun(100, func() { bodySink = bodies[0].Clone() }), 0, "DMLBody.Clone allocs")
	assert.Equal(t, rows[0], *rowSink)
	assert.Equal(t, bodies[0], *bodySink)
}
