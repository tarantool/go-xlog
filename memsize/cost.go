package memsize

const (
	memtxBlockHeaderSize = 14
	compactHeaderSize    = 10
	compactBodyMax       = 255
	compactOffsetMax     = 127

	indexExtentSize   = uint64(16 << 10)
	matrasPointerSize = uint64(8)
	treeBlockSize     = uint64(512)
	treeInnerFanout   = uint64(24)
	treeHintedLeaf    = uint64(31)
	treePlainLeaf     = uint64(62)
	hashRecordSize    = uint64(16)
	hashGrowStep      = uint64(8)
)

// TupleAllocSize returns the best-fit libsmall size class for one memtx tuple.
// Analyze additionally accounts for libsmall's stateful pool routing. The
// 14-byte block and compact overwrite follow src/box/memtx_allocator.h:45-72
// and :346-358.
func TupleAllocSize(fieldMapSize, bsize int, sc SizeClass) int {
	request := tupleRequestSize(fieldMapSize, bsize)
	if request <= 0 {
		return 0
	}

	return int(sc.Round(uint32(request))) //nolint:gosec // Tarantool tuple sizes are uint32.
}

// TreeIndexBytes returns recovered BPS-tree storage in 16 KiB matras extents.
// The 512-byte block comes from src/box/memtx_tree.cc:128-161 and bsize is the
// matras extent count in src/lib/salad/bps_tree.h:1805-1817.
func TreeIndexBytes(entries int, hinted bool) uint64 {
	if entries <= 0 {
		return 0
	}

	leafCapacity := treePlainLeaf
	if hinted {
		leafCapacity = treeHintedLeaf
	}

	levelBlocks := ceilDiv(uint64(entries), leafCapacity)

	blocks := levelBlocks

	for levelBlocks > 1 {
		levelBlocks = ceilDiv(levelBlocks, treeInnerFanout)
		blocks += levelBlocks
	}

	return matrasBytes(blocks, treeBlockSize)
}

// HashIndexBytes returns recovered linear-hash storage in 16 KiB matras
// extents. light grows by eight records (src/lib/salad/light.h:819-876), and
// memtx_hash_index_bsize reports extent count at src/box/memtx_hash.cc:279-285.
func HashIndexBytes(entries int) uint64 {
	if entries <= 0 {
		return 0
	}

	entryCount := uint64(entries)
	slots := ceilDiv(entryCount, hashGrowStep) * hashGrowStep

	return matrasBytes(slots, hashRecordSize)
}

// matrasBytes models the three-level address translator in
// small/matras.c:182-223. A non-empty matras has one root pointer extent, at
// least one second-level pointer extent, and at least one data extent; this is
// the 48 KiB floor exposed by index:bsize().
func matrasBytes(blocks, blockSize uint64) uint64 {
	if blocks == 0 {
		return 0
	}

	blocksPerExtent := indexExtentSize / blockSize
	dataExtents := ceilDiv(blocks, blocksPerExtent)
	pointersPerExtent := indexExtentSize / matrasPointerSize
	secondLevelExtents := ceilDiv(dataExtents, pointersPerExtent)

	return (1 + secondLevelExtents + dataExtents) * indexExtentSize
}

func tupleRequestSize(fieldMapSize, bsize int) int {
	if fieldMapSize < 0 || bsize < 0 {
		return 0
	}

	header := memtxBlockHeaderSize
	if bsize <= compactBodyMax && compactHeaderSize+fieldMapSize <= compactOffsetMax {
		header = compactHeaderSize
	}

	return header + fieldMapSize + bsize
}

func ceilDiv(value, divisor uint64) uint64 {
	return value/divisor + boolToUint64(value%divisor != 0)
}

func boolToUint64(value bool) uint64 {
	if value {
		return 1
	}

	return 0
}
