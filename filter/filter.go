// Package filter provides row-level predicates over format.XRow used by
// pipe.Copy to select which logical transactions to keep when streaming
// rows from a reader to a writer.
//
// Filters must NOT mutate the row. The row is logically read-only;
// callers (and pipe.Copy) rely on this when re-emitting the row downstream.
// Rows are passed by value, so a filter could not mutate the caller's row
// anyway — but the contract still holds for clarity.
//
// Filters work on header fields only. None of the constructors below
// touches BodyRaw — keeping the decode-on-demand promise of the format
// package intact.
package filter

import (
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// Filter is a row-level predicate. It returns true to keep the row.
// Implementations MUST treat the row as read-only.
type Filter func(format.XRow) bool

// FromVClock keeps rows whose LSN is strictly greater than v[r.ReplicaID].
// A replica missing from v is treated as LSN 0, so every positive LSN for
// that replica passes. Useful for "tail from a given vclock onward".
func FromVClock(v format.VClock) Filter {
	return func(r format.XRow) bool {
		// Map lookup of a missing key returns the zero value for int64 — 0.
		return r.LSN > v[r.ReplicaID]
	}
}

// UntilVClock keeps rows whose LSN is <= v[r.ReplicaID]. A replica missing
// from v is treated as LSN 0, so for that replica only LSN==0 passes
// (effectively nothing in normal logs). Inverse-ish of FromVClock — note
// the boundary: FromVClock(v) uses strict >, UntilVClock(v) uses <=, so
// the row at exactly v[id] belongs to UntilVClock.
func UntilVClock(v format.VClock) Filter {
	return func(r format.XRow) bool {
		return r.LSN <= v[r.ReplicaID]
	}
}

// ReplicaIDs keeps rows whose ReplicaID is in the supplied set. An empty
// argument list keeps nothing — this matches the "filter is a positive
// selector" intent.
func ReplicaIDs(ids ...uint32) Filter {
	// Snapshot the set into a map for O(1) lookup. The constructor cost is
	// paid once; the predicate hot path is a single map lookup.
	set := make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}

	return func(r format.XRow) bool {
		_, ok := set[r.ReplicaID]

		return ok
	}
}

// Types keeps rows whose Type is in the supplied set. Empty argument list
// keeps nothing (same convention as ReplicaIDs).
func Types(types ...iproto.Type) Filter {
	set := make(map[iproto.Type]struct{}, len(types))
	for _, t := range types {
		set[t] = struct{}{}
	}

	return func(r format.XRow) bool {
		_, ok := set[r.Type]

		return ok
	}
}

// LSNRange keeps rows whose ReplicaID == rep AND lo <= LSN <= hi (inclusive
// on both ends).
func LSNRange(rep uint32, lo, hi int64) Filter {
	return func(r format.XRow) bool {
		return r.ReplicaID == rep && r.LSN >= lo && r.LSN <= hi
	}
}

// And returns a Filter that matches when every supplied filter matches.
// Empty argument list is vacuously true (matches every row).
func And(fs ...Filter) Filter {
	return func(r format.XRow) bool {
		for _, f := range fs {
			if !f(r) {
				return false
			}
		}

		return true
	}
}

// Or returns a Filter that matches when at least one supplied filter
// matches. Empty argument list is vacuously false (matches nothing).
func Or(fs ...Filter) Filter {
	return func(r format.XRow) bool {
		for _, f := range fs {
			if f(r) {
				return true
			}
		}

		return false
	}
}

// Not returns a Filter that matches when f does not match.
func Not(f Filter) Filter {
	return func(r format.XRow) bool {
		return !f(r)
	}
}
