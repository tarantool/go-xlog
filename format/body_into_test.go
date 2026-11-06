package format_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// --- body fixtures shared by the Into tests and the benchmarks ---

// dmlInsertBody is a clean INSERT body: {SPACE_ID: 512, TUPLE: [1, "..."]}.
func dmlInsertBody() []byte {
	tuple := append([]byte{0x92, 0x01, 0xd9, 40}, make([]byte, 40)...) // [1, str8(40)]

	b := make([]byte, 0, 64)
	b = append(b, kv(uint64(iproto.IPROTO_SPACE_ID), mpUint(512))...)
	b = append(b, kv(uint64(iproto.IPROTO_TUPLE), tuple)...)

	return buildMap(2, b)
}

// dmlIndexBody is an UPDATE body on a secondary index, issued from Lua:
// {SPACE_ID: 512, INDEX_ID: 1, INDEX_BASE: 1, KEY: [1], TUPLE: [["=", 2, 3]]}.
func dmlIndexBody() []byte {
	b := make([]byte, 0, 64)
	b = append(b, kv(uint64(iproto.IPROTO_SPACE_ID), mpUint(512))...)
	b = append(b, kv(uint64(iproto.IPROTO_INDEX_ID), mpUint(1))...)
	b = append(b, kv(uint64(iproto.IPROTO_INDEX_BASE), mpUint(1))...)
	b = append(b, kv(uint64(iproto.IPROTO_KEY), []byte{0x91, 0x01})...)
	b = append(b, kv(uint64(iproto.IPROTO_TUPLE), []byte{0x91, 0x93, 0xa1, '=', 0x02, 0x03})...)

	return buildMap(5, b)
}

// dmlRichBody sets every field of a DMLBody.
func dmlRichBody() []byte {
	b := make([]byte, 0, 64)
	b = append(b, kv(uint64(iproto.IPROTO_SPACE_ID), mpUint(600))...)
	b = append(b, kv(uint64(iproto.IPROTO_INDEX_ID), mpUint(2))...)
	b = append(b, kv(uint64(iproto.IPROTO_INDEX_BASE), mpUint(1))...)
	b = append(b, kv(uint64(iproto.IPROTO_TUPLE), []byte{0x91, 0x01})...)
	b = append(b, kv(uint64(iproto.IPROTO_KEY), []byte{0x91, 0x02})...)
	b = append(b, kv(uint64(iproto.IPROTO_OPS), []byte{0x90})...)
	b = append(b, kv(uint64(iproto.IPROTO_OLD_TUPLE), []byte{0x91, 0x03})...)
	b = append(b, kv(uint64(iproto.IPROTO_NEW_TUPLE), []byte{0x91, 0x04})...)
	b = append(b, kv(uint64(iproto.IPROTO_ARROW), []byte{0xd4, 0x08, 0xff})...)
	b = append(b, kv(0x55, []byte{0x01})...)

	return buildMap(10, b)
}

// raftBody is a RAFT body with no vclock; withVClock adds one.
func raftBody(withVClock bool) []byte {
	b := make([]byte, 0, 64)
	b = append(b, kv(uint64(iproto.IPROTO_RAFT_TERM), mpUint(5))...)
	b = append(b, kv(uint64(iproto.IPROTO_RAFT_VOTE), mpUint(3))...)
	b = append(b, kv(uint64(iproto.IPROTO_RAFT_STATE), mpUint(2))...)
	b = append(b, kv(uint64(iproto.IPROTO_RAFT_LEADER_ID), mpUint(7))...)
	b = append(b, kv(uint64(iproto.IPROTO_RAFT_IS_LEADER_SEEN), []byte{0xc3})...)

	if !withVClock {
		return buildMap(5, b)
	}

	vclock := buildMap(2, append(kv(1, mpUint(100)), kv(2, mpUint(200))...))
	b = append(b, kv(uint64(iproto.IPROTO_RAFT_VCLOCK), vclock)...)
	b = append(b, kv(0x55, []byte{0x01})...)

	return buildMap(7, b)
}

// synchroConfirmBody is a CONFIRM body: {REPLICA_ID: 1, LSN: 42}.
func synchroConfirmBody() []byte {
	b := make([]byte, 0, 16)
	b = append(b, kv(uint64(iproto.IPROTO_REPLICA_ID), mpUint(1))...)
	b = append(b, kv(uint64(iproto.IPROTO_LSN), mpUint(42))...)

	return buildMap(2, b)
}

// synchroPromoteBody is a PROMOTE body with a vclock and an unknown key.
func synchroPromoteBody() []byte {
	vclock := buildMap(2, append(kv(1, mpUint(100)), kv(2, mpUint(200))...))

	b := make([]byte, 0, 32)
	b = append(b, kv(uint64(iproto.IPROTO_REPLICA_ID), mpUint(2))...)
	b = append(b, kv(uint64(iproto.IPROTO_LSN), mpUint(50))...)
	b = append(b, kv(uint64(iproto.IPROTO_TERM), mpUint(4))...)
	b = append(b, kv(uint64(iproto.IPROTO_VCLOCK), vclock)...)
	b = append(b, kv(0x55, mpUint(1))...)

	return buildMap(5, b)
}

// vyLogBody is a VYLOG body holding the record [type, {key: value...}].
func vyLogBody(typ format.VyLogType, fields ...[]byte) []byte {
	var m []byte
	for _, f := range fields {
		m = append(m, f...)
	}

	rec := append([]byte{0x92}, mpUint(uint64(typ))...)
	rec = append(rec, buildMap(len(fields), m)...)

	return buildMap(1, kv(uint64(iproto.IPROTO_TUPLE), rec))
}

// vyStmtDeleteBody is a DELETE statement of a run: {KEY: [1]}.
func vyStmtDeleteBody() []byte {
	return buildMap(1, kv(uint64(iproto.IPROTO_KEY), []byte{0x91, 0x01}))
}

// vyStmtRichBody sets every field of a VyStmtBody.
func vyStmtRichBody() []byte {
	b := make([]byte, 0, 32)
	b = append(b, kv(uint64(iproto.IPROTO_SPACE_ID), mpUint(512))...)
	b = append(b, kv(uint64(iproto.IPROTO_TUPLE), []byte{0x91, 0x01})...)
	b = append(b, kv(uint64(iproto.IPROTO_KEY), []byte{0x91, 0x02})...)
	b = append(b, kv(uint64(iproto.IPROTO_OPS), []byte{0x91, 0x90})...)
	b = append(b, kv(uint64(iproto.IPROTO_TUPLE_META), buildMap(1, kv(1, mpUint(3))))...)
	b = append(b, kv(0x55, []byte{0x01})...)

	return buildMap(6, b)
}

// pageInfoBody is a page info with every mandatory key, plus extra ones.
func pageInfoBody(extra ...[]byte) []byte {
	b := make([]byte, 0, 32)
	b = append(b, kv(1, mpUint(88))...)
	b = append(b, kv(2, mpUint(87))...)
	b = append(b, kv(3, mpUint(68))...)
	b = append(b, kv(4, mpUint(4))...)
	b = append(b, kv(5, []byte{0x91, 0x01})...)
	b = append(b, kv(6, mpUint(45))...)

	for _, e := range extra {
		b = append(b, e...)
	}

	return buildMap(6+len(extra), b)
}

// checkInto pins the contract of a Decode<X>BodyInto: it decodes what the
// allocating decode does, and decoding sparse into the dst rich was decoded
// into yields a fresh decode of sparse - no field of rich bleeds through.
// rich holds the unknown body key 0x55 with the value 0x01 and sparse none:
// OnUnknown receives exactly that, and the decode is the same without it.
func checkInto[T any](
	t *testing.T,
	decode func([]byte) (*T, error),
	into func([]byte, *T, *format.DecodeOptions) error,
	rich, sparse []byte,
) {
	t.Helper()

	for _, b := range [][]byte{rich, sparse} {
		want, err := decode(b)
		require.NoError(t, err)

		var got T

		require.NoError(t, into(b, &got, nil))
		assert.Equal(t, *want, got, "Into must decode what the allocating decoder does")
	}

	var (
		dst     T
		unknown map[uint64][]byte
	)

	require.NoError(t, into(rich, &dst, format.CollectUnknown(&unknown)))
	assert.Equal(t, map[uint64][]byte{0x55: {0x01}}, unknown, "OnUnknown gets the unknown key of rich")

	richCopy := dst

	unknown = nil

	require.NoError(t, into(sparse, &dst, format.CollectUnknown(&unknown)))
	assert.Nil(t, unknown, "sparse has no unknown key")

	want, err := decode(sparse)
	require.NoError(t, err)
	assert.NotEqual(t, *want, richCopy, "the fixtures must differ for the test to mean anything")
	assert.Equal(t, *want, dst, "sparse decoded into a reused dst must equal a fresh decode")
}

func TestDecodeDMLBodyInto_NoFieldBleed(t *testing.T) {
	t.Parallel()

	checkInto(t, format.DecodeDMLBody, format.DecodeDMLBodyInto, dmlRichBody(), dmlInsertBody())
}

func TestDecodeRaftBodyInto_NoFieldBleed(t *testing.T) {
	t.Parallel()

	checkInto(t, format.DecodeRaftBody, format.DecodeRaftBodyInto, raftBody(true), buildMap(1, kv(uint64(iproto.IPROTO_RAFT_TERM), mpUint(9))))
}

func TestDecodeSynchroBodyInto_NoFieldBleed(t *testing.T) {
	t.Parallel()

	checkInto(t, format.DecodeSynchroBody, format.DecodeSynchroBodyInto, synchroPromoteBody(), synchroConfirmBody())
}

func TestDecodeVyLogBodyInto_NoFieldBleed(t *testing.T) {
	t.Parallel()

	rich := vyLogBody(format.VyLogCreateRun, kv(uint64(format.VyKeyLSMID), mpUint(3)), kv(uint64(format.VyKeyRunID), mpUint(7)))
	rich[0] = 0x82 // fixmap(2): the record and an unknown body key.
	rich = append(rich, kv(0x55, []byte{0x01})...)
	sparse := vyLogBody(format.VyLogCreateLSM)

	checkInto(t, format.DecodeVyLogBody, format.DecodeVyLogBodyInto, rich, sparse)
}

func TestDecodeVyStmtBodyInto_NoFieldBleed(t *testing.T) {
	t.Parallel()

	checkInto(t, format.DecodeVyStmtBody, format.DecodeVyStmtBodyInto, vyStmtRichBody(), vyStmtDeleteBody())
}

func TestDecodeVyRunInfoBodyInto_NoFieldBleed(t *testing.T) {
	t.Parallel()

	rich := runInfo(kv(9, []byte{0x91, 0x01}), kv(8, buildMap(1, kv(uint64(iproto.IPROTO_INSERT), mpUint(1)))), kv(0x55, []byte{0x01}))

	checkInto(t, format.DecodeVyRunInfoBody, format.DecodeVyRunInfoBodyInto, rich, runInfo())
}

func TestDecodeVyPageInfoBodyInto_NoFieldBleed(t *testing.T) {
	t.Parallel()

	// Every key of a page info is mandatory: the sparse one differs in its
	// values, and repeats offset - the later one wins.
	sparse := pageInfoBody(kv(1, mpUint(7)))

	checkInto(t, format.DecodeVyPageInfoBody, format.DecodeVyPageInfoBodyInto, pageInfoBody(kv(0x55, []byte{0x01})), sparse)
}

// TestDecodeBodyInto_ErrorResets - an error still resets dst first, so a
// failed decode never leaves the fields of a prior row behind.
func TestDecodeBodyInto_ErrorResets(t *testing.T) {
	t.Parallel()

	dst := format.DMLBody{SpaceID: 9, IndexID: 1}

	require.ErrorIs(t, format.DecodeDMLBodyInto(nil, &dst, nil), format.ErrEmptyBody)
	assert.Equal(t, format.DMLBody{}, dst)

	vy := format.VyPageInfo{Offset: 1}

	require.ErrorIs(t, format.DecodeVyPageInfoBodyInto(buildMap(1, kv(1, mpUint(88))), &vy, nil), format.ErrVyMissingKey)
	assert.Equal(t, format.VyPageInfo{Offset: 88}, vy, "dst holds what was decoded before the error")
}

// TestDecodeBodyInto_ZeroAlloc is not parallel: AllocsPerRun counts the
// mallocs of the whole process, so a test running alongside would inflate
// them.
func TestDecodeBodyInto_ZeroAlloc(t *testing.T) { //nolint:paralleltest // AllocsPerRun counts process-wide mallocs
	if testing.Short() {
		t.Skip("allocation counts: skipped in -short mode")
	}

	var (
		dml     format.DMLBody
		raft    format.RaftBody
		synchro format.SynchroBody
		stmt    format.VyStmtBody
		runInf  format.VyRunInfo
		pageInf format.VyPageInfo
		seen    int
	)

	// An OnUnknown that stores nothing keeps the decode allocation-free.
	count := &format.DecodeOptions{OnUnknown: func(uint64, []byte) { seen++ }}

	cases := []struct {
		name string
		body []byte
		into func(b []byte) error
	}{
		{"DML clean INSERT", dmlInsertBody(), func(b []byte) error { return format.DecodeDMLBodyInto(b, &dml, nil) }},
		{"DML with INDEX_ID", dmlIndexBody(), func(b []byte) error { return format.DecodeDMLBodyInto(b, &dml, nil) }},
		{"RAFT without vclock", raftBody(false), func(b []byte) error { return format.DecodeRaftBodyInto(b, &raft, nil) }},
		{"CONFIRM", synchroConfirmBody(), func(b []byte) error { return format.DecodeSynchroBodyInto(b, &synchro, nil) }},
		{"run statement", vyStmtDeleteBody(), func(b []byte) error { return format.DecodeVyStmtBodyInto(b, &stmt, nil) }},
		{"run info", runInfo(), func(b []byte) error { return format.DecodeVyRunInfoBodyInto(b, &runInf, nil) }},
		{"page info", pageInfoBody(), func(b []byte) error { return format.DecodeVyPageInfoBodyInto(b, &pageInf, nil) }},
		// Unknown keys are skipped with no options, or handed to OnUnknown,
		// with no allocation either way.
		{"DML unknown key, nil opts", dmlRichBody(), func(b []byte) error { return format.DecodeDMLBodyInto(b, &dml, nil) }},
		{"DML unknown key, OnUnknown", dmlRichBody(), func(b []byte) error { return format.DecodeDMLBodyInto(b, &dml, count) }},
		{"synchro unknown key", buildMap(1, kv(0x55, []byte{0x01})), func(b []byte) error { return format.DecodeSynchroBodyInto(b, &synchro, count) }},
		{"run statement unknown key", vyStmtRichBody(), func(b []byte) error { return format.DecodeVyStmtBodyInto(b, &stmt, count) }},
		{"run info unknown key", runInfo(kv(0x55, []byte{0x01})), func(b []byte) error { return format.DecodeVyRunInfoBodyInto(b, &runInf, count) }},
		{"page info unknown key", pageInfoBody(kv(0x55, []byte{0x01})), func(b []byte) error { return format.DecodeVyPageInfoBodyInto(b, &pageInf, count) }},
	}

	for _, c := range cases {
		body, into := c.body, c.into
		require.NoError(t, into(body), c.name)

		allocs := testing.AllocsPerRun(100, func() {
			if err := into(body); err != nil {
				panic(err)
			}
		})
		assert.Zerof(t, allocs, "%s: allocs", c.name)
	}
}

// TestDecodeOptions_OnUnknown - OnUnknown receives every key with no field,
// in body order, with its raw value aliasing the input; a duplicate key is
// handed over each time, and CollectUnknown keeps the last.
func TestDecodeOptions_OnUnknown(t *testing.T) {
	t.Parallel()

	b := make([]byte, 0, 32)
	b = append(b, kv(0x60, mpUint(1))...)
	b = append(b, kv(uint64(iproto.IPROTO_SPACE_ID), mpUint(512))...)
	b = append(b, kv(0x61, []byte{0x91, 0x02})...)
	b = append(b, kv(0x60, mpUint(3))...)
	raw := buildMap(4, b)

	type seen struct {
		key uint64
		raw []byte
	}

	var got []seen

	var body format.DMLBody

	opts := &format.DecodeOptions{OnUnknown: func(key uint64, v []byte) { got = append(got, seen{key, v}) }}
	require.NoError(t, format.DecodeDMLBodyInto(raw, &body, opts))
	assert.Equal(t, uint32(512), body.SpaceID)
	assert.Equal(t, []seen{{0x60, []byte{0x01}}, {0x61, []byte{0x91, 0x02}}, {0x60, []byte{0x03}}}, got)

	// raw aliases the input: it is a window of the body, not a copy.
	require.Len(t, got, 3)
	assert.Same(t, &raw[len(raw)-1], &got[2].raw[0], "raw must alias the body")

	var unknown map[uint64][]byte

	require.NoError(t, format.DecodeDMLBodyInto(raw, &body, format.CollectUnknown(&unknown)))
	assert.Equal(t, map[uint64][]byte{0x60: {0x03}, 0x61: {0x91, 0x02}}, unknown)

	// An empty DecodeOptions skips unknown keys, as nil does.
	require.NoError(t, format.DecodeDMLBodyInto(raw, &body, &format.DecodeOptions{}))
	assert.Equal(t, format.DMLBody{SpaceID: 512}, body)
}
