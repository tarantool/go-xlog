// Package pipe streams logical transactions between a reader and a writer,
// optionally applying row-level filters with any-row-matches semantics.
//
// Pipelines are transaction-aware. Filtering happens at tx
// granularity — if any row inside a logical tx passes the filter, the
// entire tx is written; otherwise the entire tx is dropped. Partial
// transactions are never emitted.
//
// Copy decodes and re-encodes every tx (the writer re-runs
// assignTxIDs, which can re-assign TSN), which is required for filtering and
// row transforms. CopyRaw is the verbatim fast path: it forwards whole physical
// blocks byte-for-byte (no decode/encode/recompression/second CRC) for the
// pure copy/truncate case where no predicate is needed.
package pipe
