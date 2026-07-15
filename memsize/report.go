package memsize

import "github.com/tarantool/go-xlog/format"

// Report is the recovered memtx footprint and its comparison with the budget.
type Report struct {
	Total      uint64
	TupleBytes uint64
	IndexBytes uint64
	Reserve    uint64

	Budget      uint64
	Fits        bool
	HeadroomPct float64
	Spaces      []SpaceReport
	Warnings    []Warning
	Source      SourceInfo
}

// SpaceReport is the footprint attributed to one persistent memtx space.
type SpaceReport struct {
	ID   uint32
	Name string

	TupleCount   uint64
	TupleBytes   uint64
	PayloadBytes uint64
	FieldMapSize int
	Indexes      []IndexReport
	Bound        Bound
}

// IndexReport is the extent-quantized footprint of one memtx index.
type IndexReport struct {
	ID      uint32
	Name    string
	Type    IndexType
	Hinted  bool
	Entries uint64
	Bytes   uint64

	Estimated bool
}

// Bound describes whether a reported space is exact or one-sided.
type Bound int

const (
	// BoundExact means all modeled inputs are known.
	BoundExact Bound = iota
	// BoundLower means an excluded feature can only increase the footprint.
	BoundLower
	// BoundUpper means compression can only decrease the footprint.
	BoundUpper
)

// SourceInfo identifies the journal range included in a report.
type SourceInfo struct {
	SnapPath      string
	XlogPaths     []string
	FromVClock    format.VClock
	ToVClock      format.VClock
	Rows          uint64
	Txs           uint64
	TailTruncated bool
}

// Warning records a limitation or a configuration assumption.
type Warning struct {
	Kind   WarningKind
	Space  string
	Index  string
	Detail string
}

// WarningKind classifies analyzer limitations without requiring string parsing.
type WarningKind int

const (
	// WarnFunctionalIndex marks supplied or unavailable functional cardinality.
	WarnFunctionalIndex WarningKind = iota
	// WarnCompression marks a tuple estimate that is an upper bound.
	WarnCompression
	// WarnDataTemporary marks data absent from snapshots and xlogs.
	WarnDataTemporary
	// WarnSchemaDDL marks schema changes after the selected snapshot.
	WarnSchemaDDL
	// WarnSyncRollback marks qsync rollback rows that were not undone.
	WarnSyncRollback
	// WarnSystemAllocator marks raw malloc request sizing.
	WarnSystemAllocator
	// WarnLargeTuple marks the unmodeled large-object slab-order path.
	WarnLargeTuple
	// WarnUnmodeledIndex marks RTREE or BITSET indexes.
	WarnUnmodeledIndex
	// WarnChainGap marks a missing link in the xlog chain.
	WarnChainGap
	// WarnTailTruncated marks a trailing transaction omitted from a live xlog.
	WarnTailTruncated
	// WarnDefaultConfig marks the assumed default memtx_memory budget.
	WarnDefaultConfig
	// WarnUpdateApproximation marks UPDATE and present-key UPSERT rows whose
	// operations are treated as size-preserving in this PoC.
	WarnUpdateApproximation
	// WarnMultikeyTupleAllocation marks the per-tuple variable field-map
	// extent that Phase 5 does not add to tuple allocation bytes.
	WarnMultikeyTupleAllocation
)
