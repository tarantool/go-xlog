package memsize

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
)

const (
	arenaSlabSize = uint64(16 << 20)
	percentScale  = 100
)

type spaceTally struct {
	space              *Space
	tupleCount         uint64
	tupleBytes         uint64
	payloadBytes       uint64
	indexEntries       []uint64
	peakIndexEntries   []uint64
	entryScratch       []uint32
	hasVariableEntries bool
	bound              Bound
	largeTuple         bool
	churn              bool
	allocations        map[allocationRoute]uint64
}

type tupleMeta struct {
	request   uint32
	best      uint32
	allocated uint32
	payload   uint32
	entries   []uint32
}

type allocationRoute struct {
	best      uint32
	allocated uint32
}

type snapshotAccumulator struct {
	cfg       normalizedConfig
	allocator *smallAllocator
	spaces    map[uint32]*spaceTally
	warnings  []Warning
}

func newSnapshotAccumulator(schema *Schema, cfg normalizedConfig) *snapshotAccumulator {
	acc := &snapshotAccumulator{
		cfg:    cfg,
		spaces: make(map[uint32]*spaceTally),
	}
	if cfg.Allocator == AllocatorSmall {
		acc.allocator = newSmallAllocator(cfg.sizeClass, uint32(os.Getpagesize())) //nolint:gosec // Supported host page sizes fit uint32.
	}

	if cfg.defaultedBudget {
		acc.warn(WarnDefaultConfig, "", "", "memtx_memory not supplied; using 256 MiB")
	}

	if cfg.Allocator == AllocatorSystem {
		acc.warn(WarnSystemAllocator, "", "", "tuple bytes are raw malloc request sizes")
	}

	ids := make([]uint32, 0, len(schema.Spaces))
	for id := range schema.Spaces {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	for _, id := range ids {
		space := schema.Spaces[id]

		if space.Engine == "memtx" && space.Kind == SpaceDataTemporary {
			acc.warn(WarnDataTemporary, space.Name, "", "tuples are not persisted; report is a lower bound")
		}

		if !space.IsMemtxData() {
			continue
		}

		bound := BoundExact
		if space.Compressed {
			bound = BoundUpper

			acc.warn(WarnCompression, space.Name, "", "snapshot payload is decompressed; tuple bytes are an upper bound")
		}

		for i := range space.Indexes {
			if !space.Indexes[i].Multikey {
				continue
			}

			if bound == BoundExact {
				bound = BoundLower
			}

			acc.warn(WarnMultikeyTupleAllocation, space.Name, "",
				"dynamic multikey field-map extent is excluded from tuple bytes")

			break
		}

		tally := &spaceTally{
			space:            space,
			indexEntries:     make([]uint64, len(space.Indexes)),
			peakIndexEntries: make([]uint64, len(space.Indexes)),
			bound:            bound,
		}

		for i := range space.Indexes {
			index := &space.Indexes[i]
			if !index.Functional && (index.Multikey || index.ExcludeNull) {
				tally.hasVariableEntries = true
			}
		}

		if tally.hasVariableEntries {
			tally.entryScratch = make([]uint32, len(space.Indexes))
		}

		if cfg.Allocator == AllocatorSmall {
			tally.allocations = make(map[allocationRoute]uint64)
		}

		acc.spaces[id] = tally
	}

	return acc
}

func (a *snapshotAccumulator) addTuple(spaceID uint32, tuple []byte) (tupleMeta, bool, error) {
	meta, ok, err := a.allocateTuple(spaceID, tuple)
	if err != nil {
		return tupleMeta{}, false, err
	}

	if !ok {
		return tupleMeta{}, false, nil
	}

	a.addAllocatedTuple(spaceID, meta, false)

	return meta, true, nil
}

func (a *snapshotAccumulator) allocateTuple(spaceID uint32, tuple []byte) (tupleMeta, bool, error) {
	tally := a.spaces[spaceID]
	if tally == nil {
		return tupleMeta{}, false, nil
	}

	entries, err := tupleIndexEntryCounts(tally, tuple)
	if err != nil {
		return tupleMeta{}, false, err
	}

	bsize := len(tuple)
	request := tupleRequestSize(tally.space.FieldMapSize, bsize)
	request32 := uint32(request) //nolint:gosec // Tarantool tuple allocation requests are uint32.

	best, allocated := request32, request32
	if a.cfg.Allocator == AllocatorSmall {
		best = a.cfg.sizeClass.Round(request32)
		allocated = a.allocator.allocate(request32)
		tally.allocations[allocationRoute{best: best, allocated: allocated}]++
	}

	if bsize > maximumModeledTupleSize && !tally.largeTuple {
		tally.largeTuple = true
		a.warn(WarnLargeTuple, tally.space.Name, "",
			fmt.Sprintf("%d-byte payload exceeds the default memtx_max_tuple_size", bsize))
	}

	return tupleMeta{
		request:   request32,
		best:      best,
		allocated: allocated,
		payload:   uint32(bsize), //nolint:gosec // Tarantool tuple bodies are uint32-sized.
		entries:   entries,
	}, true, nil
}

func (a *snapshotAccumulator) addAllocatedTuple(spaceID uint32, meta tupleMeta, churn bool) {
	tally := a.spaces[spaceID]
	if tally == nil {
		return
	}

	tally.tupleCount++
	tally.payloadBytes += uint64(meta.payload)
	tally.tupleBytes += uint64(meta.allocated)
	tally.churn = tally.churn || churn

	for indexNo := range tally.space.Indexes {
		index := &tally.space.Indexes[indexNo]
		if index.Functional {
			continue
		}

		entries := tupleIndexEntries(tally, meta, indexNo)
		tally.indexEntries[indexNo] += entries
		tally.peakIndexEntries[indexNo] = max(tally.peakIndexEntries[indexNo], tally.indexEntries[indexNo])
	}
}

func (a *snapshotAccumulator) replaceTuple(spaceID uint32, old, replacement tupleMeta) {
	tally := a.spaces[spaceID]
	if tally == nil {
		return
	}

	tally.payloadBytes = tally.payloadBytes - uint64(old.payload) + uint64(replacement.payload)
	tally.tupleBytes = tally.tupleBytes - uint64(old.allocated) + uint64(replacement.allocated)
	tally.churn = true

	for indexNo := range tally.space.Indexes {
		index := &tally.space.Indexes[indexNo]
		if index.Functional || !index.Multikey && !index.ExcludeNull {
			continue
		}

		oldEntries := tupleIndexEntries(tally, old, indexNo)
		newEntries := tupleIndexEntries(tally, replacement, indexNo)

		if tally.indexEntries[indexNo] < oldEntries {
			panic("memsize: index entry count underflow")
		}

		tally.indexEntries[indexNo] = tally.indexEntries[indexNo] - oldEntries + newEntries
		tally.peakIndexEntries[indexNo] = max(tally.peakIndexEntries[indexNo], tally.indexEntries[indexNo])
	}

	a.releaseTuple(tally, old)
}

func (a *snapshotAccumulator) removeTuple(spaceID uint32, meta tupleMeta) {
	tally := a.spaces[spaceID]
	if tally == nil {
		return
	}

	tally.tupleCount--
	tally.payloadBytes -= uint64(meta.payload)
	tally.tupleBytes -= uint64(meta.allocated)
	tally.churn = true

	for indexNo := range tally.space.Indexes {
		index := &tally.space.Indexes[indexNo]
		if index.Functional {
			continue
		}

		entries := tupleIndexEntries(tally, meta, indexNo)
		if tally.indexEntries[indexNo] < entries {
			panic("memsize: index entry count underflow")
		}

		tally.indexEntries[indexNo] -= entries
	}

	a.releaseTuple(tally, meta)
}

func (a *snapshotAccumulator) clearSpace(spaceID uint32) {
	tally := a.spaces[spaceID]
	if tally == nil {
		return
	}

	if a.cfg.Allocator == AllocatorSmall {
		for route, count := range tally.allocations {
			a.allocator.release(route.best, route.allocated, count)
		}

		clear(tally.allocations)
	}

	tally.tupleCount = 0
	tally.tupleBytes = 0
	tally.payloadBytes = 0
	clear(tally.indexEntries)
	clear(tally.peakIndexEntries)
	tally.churn = true
}

func (a *snapshotAccumulator) markSpaceChurn(spaceID uint32) {
	if tally := a.spaces[spaceID]; tally != nil {
		tally.churn = true
	}
}

func (a *snapshotAccumulator) markUpperBound() {
	for _, tally := range a.spaces {
		tally.bound = BoundUpper
	}
}

func (a *snapshotAccumulator) releaseTuple(tally *spaceTally, meta tupleMeta) {
	if a.cfg.Allocator != AllocatorSmall {
		return
	}

	route := allocationRoute{best: meta.best, allocated: meta.allocated}
	count := tally.allocations[route]

	if count == 0 {
		panic("memsize: tuple allocation route underflow")
	}

	if count == 1 {
		delete(tally.allocations, route)
	} else {
		tally.allocations[route] = count - 1
	}

	a.allocator.release(meta.best, meta.allocated, 1)
}

func (a *snapshotAccumulator) buildReport(source SourceInfo) *Report {
	report := &Report{
		Budget:   a.cfg.budget,
		Warnings: a.warnings,
		Source:   source,
		Spaces:   make([]SpaceReport, 0, len(a.spaces)),
	}

	ids := make([]uint32, 0, len(a.spaces))
	for id := range a.spaces {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	for _, id := range ids {
		spaceReport := a.buildSpaceReport(a.spaces[id])

		report.TupleBytes += spaceReport.TupleBytes

		for _, index := range spaceReport.Indexes {
			report.IndexBytes += index.Bytes
		}

		report.Spaces = append(report.Spaces, spaceReport)
	}

	report.Warnings = a.warnings
	report.Total = report.TupleBytes + report.IndexBytes

	report.Reserve = slabReserve(report.IndexBytes)
	if a.cfg.Allocator == AllocatorSmall {
		report.Reserve += slabReserve(report.TupleBytes)
	}

	usedWithReserve := report.Total + report.Reserve

	report.Fits = usedWithReserve <= report.Budget
	if report.Fits {
		report.HeadroomPct = float64(report.Budget-usedWithReserve) / float64(report.Budget) * percentScale
	} else {
		report.HeadroomPct = -float64(usedWithReserve-report.Budget) / float64(report.Budget) * percentScale
	}

	return report
}

func (a *snapshotAccumulator) buildSpaceReport(tally *spaceTally) SpaceReport {
	report := SpaceReport{
		ID:           tally.space.ID,
		Name:         tally.space.Name,
		TupleCount:   tally.tupleCount,
		TupleBytes:   tally.tupleBytes,
		PayloadBytes: tally.payloadBytes,
		FieldMapSize: tally.space.FieldMapSize,
		Indexes:      make([]IndexReport, 0, len(tally.space.Indexes)),
		Bound:        tally.bound,
	}

	for indexNo := range tally.space.Indexes {
		index := &tally.space.Indexes[indexNo]

		indexReport := a.buildIndexReport(tally, indexNo, index)
		if indexReport.Estimated && index.Functional && indexReport.Entries == 0 {
			report.Bound = BoundLower
		}

		if index.Type == IndexRTree || index.Type == IndexBitset {
			report.Bound = BoundLower
		}

		report.Indexes = append(report.Indexes, indexReport)
	}

	return report
}

func (a *snapshotAccumulator) buildIndexReport(tally *spaceTally, indexNo int, index *Index) IndexReport {
	entries := tally.indexEntries[indexNo]
	estimated := false

	if index.Functional {
		estimated = true
		key := tally.space.Name + "." + index.Name

		keysPerTuple, ok := a.cfg.FuncIndexKeysPerTuple[key]
		if !ok {
			entries = 0

			a.warn(WarnFunctionalIndex, tally.space.Name, index.Name,
				"excluded because functional cardinality was not supplied")
		} else {
			entries = roundedCardinality(tally.tupleCount, keysPerTuple)
			a.warn(WarnFunctionalIndex, tally.space.Name, index.Name,
				"using "+strconv.FormatFloat(keysPerTuple, 'g', -1, 64)+" keys per tuple")
		}
	}

	sizingEntries := entries
	bytes := uint64(0)
	entryCount := uint64ToInt(sizingEntries)

	switch index.Type {
	case IndexTree:
		estimated = estimated || tally.churn
		bytes = TreeIndexBytes(entryCount, index.Hinted)
	case IndexHash:
		if !index.Functional {
			sizingEntries = tally.peakIndexEntries[indexNo]
			entryCount = uint64ToInt(sizingEntries)
		}

		bytes = HashIndexBytes(entryCount)
	case IndexRTree, IndexBitset:
		estimated = true

		a.warn(WarnUnmodeledIndex, tally.space.Name, index.Name, "index bytes excluded")
	}

	return IndexReport{
		ID:        index.ID,
		Name:      index.Name,
		Type:      index.Type,
		Hinted:    index.Hinted,
		Entries:   entries,
		Bytes:     bytes,
		Estimated: estimated,
	}
}

func tupleIndexEntryCounts(tally *spaceTally, tuple []byte) ([]uint32, error) {
	if !tally.hasVariableEntries {
		return nil, nil
	}

	clear(tally.entryScratch)

	for indexNo := range tally.space.Indexes {
		index := &tally.space.Indexes[indexNo]
		if index.Functional || !index.Multikey && !index.ExcludeNull {
			continue
		}

		entries, err := indexEntryCount(tuple, index)
		if err != nil {
			return nil, fmt.Errorf("index %q: %w", index.Name, err)
		}

		tally.entryScratch[indexNo] = entries
	}

	return tally.entryScratch, nil
}

func tupleIndexEntries(tally *spaceTally, meta tupleMeta, indexNo int) uint64 {
	index := &tally.space.Indexes[indexNo]
	if !index.Multikey && !index.ExcludeNull {
		return 1
	}

	if len(meta.entries) != len(tally.space.Indexes) {
		panic("memsize: missing per-index tuple cardinality")
	}

	return uint64(meta.entries[indexNo])
}

func (m tupleMeta) retain() tupleMeta {
	m.entries = slices.Clone(m.entries)

	return m
}

func (a *snapshotAccumulator) warn(kind WarningKind, space, index, detail string) {
	a.warnings = append(a.warnings, Warning{
		Kind:   kind,
		Space:  space,
		Index:  index,
		Detail: detail,
	})
}

func roundedCardinality(tuples uint64, perTuple float64) uint64 {
	cardinality := math.Round(float64(tuples) * perTuple)
	if cardinality >= float64(math.MaxUint64) {
		return math.MaxUint64
	}

	return uint64(cardinality)
}

func uint64ToInt(value uint64) int {
	maxInt := uint64(^uint(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}

	return int(value)
}

func slabReserve(used uint64) uint64 {
	if used == 0 {
		return 0
	}

	return ceilDiv(used, arenaSlabSize)*arenaSlabSize - used
}
