package filter_test

import (
	"fmt"

	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/filter"
	"github.com/tarantool/go-xlog/format"
)

// sampleRows is a small fixed set of header-only rows used by the filter
// examples. Filters look at header fields only, so BodyRaw is left nil.
func sampleRows() []format.XRow {
	return []format.XRow{
		{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: 1},
		{Type: iproto.IPROTO_REPLACE, ReplicaID: 1, LSN: 2},
		{Type: iproto.IPROTO_DELETE, ReplicaID: 2, LSN: 1},
		{Type: iproto.IPROTO_INSERT, ReplicaID: 2, LSN: 2},
		{Type: iproto.IPROTO_REPLACE, ReplicaID: 1, LSN: 3},
	}
}

// kept applies f to every sample row and returns a formatted line for each row
// it keeps, so each example can print exactly which rows the predicate selects.
func kept(f filter.Filter) []string {
	var lines []string

	for _, r := range sampleRows() {
		if f(r) {
			lines = append(lines, fmt.Sprintf("%-7s replica=%d lsn=%d", format.TypeName(r.Type), r.ReplicaID, r.LSN))
		}
	}

	return lines
}

// ExampleTypes keeps only rows whose request type is in the given set.
func ExampleTypes() {
	for _, s := range kept(filter.Types(iproto.IPROTO_INSERT)) {
		fmt.Println(s)
	}

	// Output:
	// INSERT  replica=1 lsn=1
	// INSERT  replica=2 lsn=2
}

// ExampleReplicaIDs keeps only rows originating from the given replica ids.
func ExampleReplicaIDs() {
	for _, s := range kept(filter.ReplicaIDs(2)) {
		fmt.Println(s)
	}

	// Output:
	// DELETE  replica=2 lsn=1
	// INSERT  replica=2 lsn=2
}

// ExampleLSNRange keeps rows for one replica whose LSN falls in an inclusive
// [lo, hi] window.
func ExampleLSNRange() {
	for _, s := range kept(filter.LSNRange(1, 2, 3)) {
		fmt.Println(s)
	}

	// Output:
	// REPLACE replica=1 lsn=2
	// REPLACE replica=1 lsn=3
}

// ExampleAnd composes predicates: keep only INSERT/REPLACE rows from replica 1.
// And matches a row only when every supplied filter matches it.
func ExampleAnd() {
	for _, s := range kept(filter.And(
		filter.ReplicaIDs(1),
		filter.Types(iproto.IPROTO_INSERT, iproto.IPROTO_REPLACE),
	)) {
		fmt.Println(s)
	}

	// Output:
	// INSERT  replica=1 lsn=1
	// REPLACE replica=1 lsn=2
	// REPLACE replica=1 lsn=3
}

// ExampleOr keeps a row when at least one supplied filter matches it.
func ExampleOr() {
	for _, s := range kept(filter.Or(
		filter.Types(iproto.IPROTO_DELETE),
		filter.ReplicaIDs(2),
	)) {
		fmt.Println(s)
	}

	// Output:
	// DELETE  replica=2 lsn=1
	// INSERT  replica=2 lsn=2
}

// ExampleNot inverts a predicate: keep every row that is NOT a DELETE.
func ExampleNot() {
	for _, s := range kept(filter.Not(filter.Types(iproto.IPROTO_DELETE))) {
		fmt.Println(s)
	}

	// Output:
	// INSERT  replica=1 lsn=1
	// REPLACE replica=1 lsn=2
	// INSERT  replica=2 lsn=2
	// REPLACE replica=1 lsn=3
}

// ExampleFromVClock keeps rows strictly after a per-replica position — the
// "tail from a given vclock onward" selector. Here replica 1 is advanced past
// LSN 2 (so only its LSN 3 survives) while replica 2 is absent from the vclock
// and therefore treated as LSN 0 (so all of its positive LSNs pass).
func ExampleFromVClock() {
	for _, s := range kept(filter.FromVClock(format.VClock{1: 2})) {
		fmt.Println(s)
	}

	// Output:
	// DELETE  replica=2 lsn=1
	// INSERT  replica=2 lsn=2
	// REPLACE replica=1 lsn=3
}
