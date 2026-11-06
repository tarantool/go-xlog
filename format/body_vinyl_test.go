package format_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

func TestDecodeVyStmtBody_AllKeys(t *testing.T) {
	t.Parallel()

	b := make([]byte, 0, 32)
	b = append(b, kv(uint64(iproto.IPROTO_SPACE_ID), mpUint(512))...)
	b = append(b, kv(uint64(iproto.IPROTO_TUPLE), []byte{0x91, 0x01})...)
	b = append(b, kv(uint64(iproto.IPROTO_KEY), []byte{0x91, 0x02})...)
	b = append(b, kv(uint64(iproto.IPROTO_OPS), []byte{0x91, 0x90})...)
	// tuple_meta: {flags: DEFERRED_DELETE | SKIP_READ, 7: 7} - unknown meta
	// keys are ignored.
	b = append(b, kv(uint64(iproto.IPROTO_TUPLE_META), buildMap(2, append(kv(1, mpUint(3)), kv(7, mpUint(7))...)))...)
	b = append(b, kv(0x55, []byte{0x01})...)

	var (
		body    format.VyStmtBody
		unknown map[uint64][]byte
	)

	require.NoError(t, format.DecodeVyStmtBodyInto(buildMap(6, b), &body, format.CollectUnknown(&unknown)))
	assert.Equal(t, format.VyStmtBody{
		SpaceID: 512,
		Tuple:   []byte{0x91, 0x01},
		Key:     []byte{0x91, 0x02},
		Ops:     []byte{0x91, 0x90},
		Flags:   format.VyStmtDeferredDelete | format.VyStmtSkipRead,
	}, body)
	assert.Equal(t, map[uint64][]byte{0x55: {0x01}}, unknown, "unknown body keys")
}

func TestDecodeVyStmtBody_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"not a map", []byte{0x01}},
		{"bad key", []byte{0x81, 0xcc}},
		{"truncated value", append(mpFixmap(1), kv(0x55, []byte{0xdc, 0x00})...)},
		{"bad space_id", buildMap(1, kv(uint64(iproto.IPROTO_SPACE_ID), []byte{0xa1, 'x'}))},
		{"tuple_meta not a map", buildMap(1, kv(uint64(iproto.IPROTO_TUPLE_META), mpUint(1)))},
		{"bad flags", buildMap(1, kv(uint64(iproto.IPROTO_TUPLE_META), buildMap(1, kv(1, []byte{0xa1, 'x'}))))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := format.DecodeVyStmtBody(tc.raw)
			require.Error(t, err)
		})
	}

	_, err := format.DecodeVyStmtBody(nil)
	require.ErrorIs(t, err, format.ErrEmptyBody)
}

// runInfo builds a run info body with the mandatory keys and the extra ones.
func runInfo(extra ...[]byte) []byte {
	b := make([]byte, 0, 64)
	b = append(b, kv(1, []byte{0x91, 0x01})...) // min_key
	b = append(b, kv(2, []byte{0x91, 0x09})...) // max_key
	b = append(b, kv(3, mpUint(4))...)          // min_lsn
	b = append(b, kv(4, mpUint(12))...)         // max_lsn
	b = append(b, kv(5, mpUint(2))...)          // page_count

	for _, e := range extra {
		b = append(b, e...)
	}

	return buildMap(5+len(extra), b)
}

func TestDecodeVyRunInfoBody_AllKeys(t *testing.T) {
	t.Parallel()

	stat := make([]byte, 0, 16)
	stat = append(stat, kv(uint64(iproto.IPROTO_INSERT), mpUint(1))...)
	stat = append(stat, kv(uint64(iproto.IPROTO_REPLACE), mpUint(2))...)
	stat = append(stat, kv(uint64(iproto.IPROTO_DELETE), mpUint(3))...)
	stat = append(stat, kv(uint64(iproto.IPROTO_UPSERT), mpUint(4))...)
	stat = append(stat, kv(uint64(iproto.IPROTO_UPDATE), mpUint(5))...) // ignored

	var (
		info    format.VyRunInfo
		unknown map[uint64][]byte
	)

	require.NoError(t, format.DecodeVyRunInfoBodyInto(runInfo(
		kv(9, []byte{0x91, 0x01}),
		kv(8, buildMap(5, stat)),
		kv(0x55, []byte{0x01}),
	), &info, format.CollectUnknown(&unknown)))
	assert.Equal(t, format.VyRunInfo{
		MinKey:       []byte{0x91, 0x01},
		MaxKey:       []byte{0x91, 0x09},
		MinLSN:       4,
		MaxLSN:       12,
		PageCount:    2,
		BloomVersion: 3,
		Bloom:        []byte{0x91, 0x01},
		StmtStat:     format.VyStmtStat{Inserts: 1, Replaces: 2, Deletes: 3, Upserts: 4},
	}, info)
	assert.Equal(t, map[uint64][]byte{0x55: {0x01}}, unknown, "unknown body keys")
}

func TestDecodeVyRunInfoBody_LegacyBloom(t *testing.T) {
	t.Parallel()

	for key, version := range map[uint64]uint8{6: 1, 7: 2} {
		info, err := format.DecodeVyRunInfoBody(runInfo(kv(key, []byte{0x90})))
		require.NoError(t, err)
		assert.Equal(t, version, info.BloomVersion)
	}

	info, err := format.DecodeVyRunInfoBody(runInfo())
	require.NoError(t, err)
	assert.Zero(t, info.BloomVersion)
	assert.Nil(t, info.Bloom)
}

func TestDecodeVyRunInfoBody_Errors(t *testing.T) {
	t.Parallel()

	_, err := format.DecodeVyRunInfoBody(nil)
	require.ErrorIs(t, err, format.ErrEmptyBody)

	_, err = format.DecodeVyRunInfoBody(buildMap(1, kv(2, []byte{0x90})))
	require.ErrorIs(t, err, format.ErrVyMissingKey)
	require.ErrorContains(t, err, "min_key")

	_, err = format.DecodeVyRunInfoBody(buildMap(1, kv(3, []byte{0xa1, 'x'})))
	require.Error(t, err)

	_, err = format.DecodeVyRunInfoBody(runInfo(kv(8, mpUint(1))))
	require.Error(t, err)

	_, err = format.DecodeVyRunInfoBody(runInfo(kv(8, buildMap(1, kv(2, []byte{0xa1, 'x'})))))
	require.Error(t, err)
}

func TestDecodeVyPageInfoBody_AllKeys(t *testing.T) {
	t.Parallel()

	b := make([]byte, 0, 32)
	b = append(b, kv(1, mpUint(88))...)
	b = append(b, kv(2, mpUint(87))...)
	b = append(b, kv(3, mpUint(68))...)
	b = append(b, kv(4, mpUint(4))...)
	b = append(b, kv(5, []byte{0x91, 0x01})...)
	b = append(b, kv(6, mpUint(45))...)
	b = append(b, kv(0x55, []byte{0x01})...)

	var (
		page    format.VyPageInfo
		unknown map[uint64][]byte
	)

	require.NoError(t, format.DecodeVyPageInfoBodyInto(buildMap(7, b), &page, format.CollectUnknown(&unknown)))
	assert.Equal(t, format.VyPageInfo{
		Offset:         88,
		Size:           87,
		UnpackedSize:   68,
		RowCount:       4,
		MinKey:         []byte{0x91, 0x01},
		RowIndexOffset: 45,
	}, page)
	assert.Equal(t, map[uint64][]byte{0x55: {0x01}}, unknown, "unknown body keys")
}

func TestDecodeVyPageInfoBody_Errors(t *testing.T) {
	t.Parallel()

	_, err := format.DecodeVyPageInfoBody(nil)
	require.ErrorIs(t, err, format.ErrEmptyBody)

	_, err = format.DecodeVyPageInfoBody(buildMap(1, kv(1, mpUint(88))))
	require.ErrorIs(t, err, format.ErrVyMissingKey)
	require.ErrorContains(t, err, "size")

	_, err = format.DecodeVyPageInfoBody(buildMap(1, kv(4, []byte{0xa1, 'x'})))
	require.Error(t, err)
}

func TestDecodeVyRowIndexBody(t *testing.T) {
	t.Parallel()

	data := []byte{0xc4, 8, 0, 0, 0, 0, 0, 0, 0x01, 0x02}
	offsets, err := format.DecodeVyRowIndexBody(buildMap(2, append(kv(7, mpUint(1)), kv(1, data)...)))
	require.NoError(t, err)
	assert.Equal(t, []uint32{0, 0x0102}, offsets)

	for name, bin := range map[string][]byte{
		"bin16": append([]byte{0xc5, 0, 4}, 0, 0, 0, 5),
		"bin32": append([]byte{0xc6, 0, 0, 0, 4}, 0, 0, 0, 5),
	} {
		offsets, err := format.DecodeVyRowIndexBody(buildMap(1, kv(1, bin)))
		require.NoError(t, err, name)
		assert.Equal(t, []uint32{5}, offsets, name)
	}
}

func TestDecodeVyRowIndexBody_Errors(t *testing.T) {
	t.Parallel()

	_, err := format.DecodeVyRowIndexBody(nil)
	require.ErrorIs(t, err, format.ErrEmptyBody)

	_, err = format.DecodeVyRowIndexBody(buildMap(1, kv(1, []byte{0xc4, 3, 0, 0, 0})))
	require.ErrorIs(t, err, format.ErrVyRowIndex)

	for name, bin := range map[string][]byte{
		"not a bin":   mpUint(1),
		"short bin8":  {0xc4},
		"short bin16": {0xc5, 0},
		"short bin32": {0xc6, 0, 0},
		"short data":  {0xc4, 4, 0},
	} {
		_, err := format.DecodeVyRowIndexBody(append(mpFixmap(1), append([]byte{0x01}, bin...)...))
		require.Error(t, err, name)
	}
}
