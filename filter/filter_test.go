package filter_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/filter"
	"github.com/tarantool/go-xlog/format"
)

func row(replicaID uint32, lsn int64, typ iproto.Type) format.XRow {
	return format.XRow{
		ReplicaID: replicaID,
		LSN:       lsn,
		Type:      typ,
	}
}

// TestFromVClock — keeps rows whose r.LSN > v[r.ReplicaID]. Missing replicas
// in v are treated as 0 (everything passes for that replica).
func TestFromVClock(t *testing.T) {
	t.Parallel()

	f := filter.FromVClock(format.VClock{1: 10})

	cases := []struct {
		r    format.XRow
		want bool
	}{
		{row(1, 9, iproto.IPROTO_INSERT), false},
		{row(1, 10, iproto.IPROTO_INSERT), false}, // Strict >.
		{row(1, 11, iproto.IPROTO_INSERT), true},
		// Missing replica 2 in v → v[2]==0 → any LSN>0 passes.
		{row(2, 1, iproto.IPROTO_INSERT), true},
		{row(2, 0, iproto.IPROTO_INSERT), false},
	}
	for i, c := range cases {
		assert.Equal(t, c.want, f(c.r), "case %d (rep=%d lsn=%d)", i, c.r.ReplicaID, c.r.LSN)
	}
}

// TestUntilVClock — keeps rows whose r.LSN <= v[r.ReplicaID]. Missing
// replicas in v are treated as 0 (nothing passes for that replica unless
// LSN is also 0).
func TestUntilVClock(t *testing.T) {
	t.Parallel()

	f := filter.UntilVClock(format.VClock{1: 10})

	cases := []struct {
		r    format.XRow
		want bool
	}{
		{row(1, 9, iproto.IPROTO_INSERT), true},
		{row(1, 10, iproto.IPROTO_INSERT), true},
		{row(1, 11, iproto.IPROTO_INSERT), false},
		{row(2, 1, iproto.IPROTO_INSERT), false},
		{row(2, 0, iproto.IPROTO_INSERT), true},
	}
	for i, c := range cases {
		assert.Equal(t, c.want, f(c.r), "case %d (rep=%d lsn=%d)", i, c.r.ReplicaID, c.r.LSN)
	}
}

func TestReplicaIDs(t *testing.T) {
	t.Parallel()

	f := filter.ReplicaIDs(1, 3)
	assert.True(t, f(row(1, 0, 0)), "replica 1 should match")
	assert.True(t, f(row(3, 0, 0)), "replica 3 should match")
	assert.False(t, f(row(2, 0, 0)), "replica 2 should not match")

	// Empty arg list: no replica matches.
	empty := filter.ReplicaIDs()
	assert.False(t, empty(row(1, 0, 0)), "empty ReplicaIDs() should match nothing")
}

func TestTypes(t *testing.T) {
	t.Parallel()

	f := filter.Types(iproto.IPROTO_INSERT, iproto.IPROTO_DELETE)
	assert.True(t, f(row(0, 0, iproto.IPROTO_INSERT)), "insert should match")
	assert.True(t, f(row(0, 0, iproto.IPROTO_DELETE)), "delete should match")
	assert.False(t, f(row(0, 0, iproto.IPROTO_REPLACE)), "replace should not match")
}

func TestLSNRange(t *testing.T) {
	t.Parallel()

	f := filter.LSNRange(1, 5, 10)

	cases := []struct {
		r    format.XRow
		want bool
	}{
		{row(1, 5, 0), true},
		{row(1, 7, 0), true},
		{row(1, 10, 0), true},
		{row(1, 4, 0), false},
		{row(1, 11, 0), false},
		{row(2, 7, 0), false}, // Wrong replica.
	}
	for i, c := range cases {
		assert.Equal(t, c.want, f(c.r), "case %d", i)
	}
}

func TestAnd(t *testing.T) {
	t.Parallel()

	f := filter.And(filter.ReplicaIDs(1), filter.Types(iproto.IPROTO_INSERT))
	assert.True(t, f(row(1, 0, iproto.IPROTO_INSERT)), "rep=1 + insert should match And")
	assert.False(t, f(row(1, 0, iproto.IPROTO_DELETE)), "rep=1 + delete should not match And")
	assert.False(t, f(row(2, 0, iproto.IPROTO_INSERT)), "rep=2 + insert should not match And")
	// Empty And: vacuously true.
	assert.True(t, filter.And()(row(0, 0, 0)), "empty And() should be vacuously true")
}

func TestOr(t *testing.T) {
	t.Parallel()

	f := filter.Or(filter.ReplicaIDs(1), filter.Types(iproto.IPROTO_INSERT))
	assert.True(t, f(row(1, 0, iproto.IPROTO_DELETE)), "rep=1 should match Or")
	assert.True(t, f(row(2, 0, iproto.IPROTO_INSERT)), "insert should match Or")
	assert.False(t, f(row(2, 0, iproto.IPROTO_DELETE)), "rep=2 + delete should not match Or")
	// Empty Or: vacuously false.
	assert.False(t, filter.Or()(row(0, 0, 0)), "empty Or() should be vacuously false")
}

func TestNot(t *testing.T) {
	t.Parallel()

	f := filter.Not(filter.ReplicaIDs(1))
	assert.False(t, f(row(1, 0, 0)), "Not(ReplicaIDs(1)) should drop replica 1")
	assert.True(t, f(row(2, 0, 0)), "Not(ReplicaIDs(1)) should keep replica 2")
}

// TestFilters_NoMutation — filters must not mutate the row. Build one row,
// snapshot it, pass it through every filter, assert all fields unchanged.
func TestFilters_NoMutation(t *testing.T) {
	t.Parallel()

	r := format.XRow{
		Type:      iproto.IPROTO_INSERT,
		ReplicaID: 1,
		GroupID:   2,
		LSN:       42,
		TSN:       40,
		Timestamp: 123.456,
		Flags:     iproto.IPROTO_FLAG_COMMIT | iproto.IPROTO_FLAG_WAIT_SYNC,
		BodyRaw:   []byte{0x80}, // Empty msgpack map.
		StreamID:  7,
	}
	snapshot := r
	bodyCopy := append([]byte(nil), r.BodyRaw...)

	fs := []filter.Filter{
		filter.FromVClock(format.VClock{1: 10}),
		filter.UntilVClock(format.VClock{1: 100}),
		filter.ReplicaIDs(1, 2, 3),
		filter.Types(iproto.IPROTO_INSERT),
		filter.LSNRange(1, 1, 1000),
		filter.And(filter.ReplicaIDs(1), filter.Types(iproto.IPROTO_INSERT)),
		filter.Or(filter.ReplicaIDs(99), filter.Types(iproto.IPROTO_INSERT)),
		filter.Not(filter.ReplicaIDs(99)),
	}
	for i, f := range fs {
		_ = f(r)
		assert.True(t, reflect.DeepEqual(r, snapshot), "filter %d mutated row header fields", i)
		assert.True(t, reflect.DeepEqual(r.BodyRaw, bodyCopy), "filter %d mutated row body", i)
	}
}
