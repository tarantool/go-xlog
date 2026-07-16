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
