package memsize

import "math/bits"

const (
	smallMempoolMax       = 1024
	smallPoolsPerGroupMax = 32
	smallMslabSize        = uint64(112)
	smallPointerSize      = uint64(8)
	smallObjectDivisor    = uint64(16)
	smallWasteDivisor     = uint64(4)
	mempoolOverheadRatio  = 0.01
	slabCacheOrderMax     = 15
)

type smallAllocator struct {
	classes   SizeClass
	objectMax uint32
	pools     []smallPool
	groups    []smallPoolGroup
}

type smallPool struct {
	objectSize  uint32
	group       uint16
	groupOffset uint8
	waste       uint64
}

type smallPoolGroup struct {
	first      uint16
	activeMask uint32
	wasteMax   uint64
}

// newSmallAllocator builds the pool routing layer initialized by
// small_alloc_create(). Pool grouping and activation follow
// src/lib/small/small/small.c:147-291; the 112-byte mslab header and one-percent
// overhead target come from src/lib/small/include/small/mempool.h:106-145.
func newSmallAllocator(classes SizeClass, pageSize uint32) *smallAllocator {
	if pageSize == 0 || pageSize&(pageSize-1) != 0 || uint64(pageSize) > arenaSlabSize {
		panic("memsize: allocator page size must be a power of two no larger than the arena slab")
	}

	granularity := uint64(1) << classes.ignoreBits
	objectMax := (arenaSlabSize - smallMslabSize) / smallObjectDivisor
	objectMax &^= smallPointerSize - 1
	objectMax = alignUp(objectMax, granularity)

	allocator := &smallAllocator{
		classes:   classes,
		objectMax: uint32(objectMax), //nolint:gosec // A 16 MiB arena bounds the result below uint32.
		pools:     make([]smallPool, 0, smallMempoolMax),
		groups:    make([]smallPoolGroup, 0, smallMempoolMax),
	}

	orders := make([]uint8, 0, smallMempoolMax)
	for class := uint32(0); len(allocator.pools) < smallMempoolMax; class++ {
		objectSize := classes.classSize(class)
		objectSize = min(objectSize, allocator.objectMax)

		allocator.pools = append(allocator.pools, smallPool{objectSize: objectSize})

		orders = append(orders, mempoolSlabOrder(objectSize, pageSize))
		if objectSize == allocator.objectMax {
			break
		}
	}

	allocator.objectMax = allocator.pools[len(allocator.pools)-1].objectSize
	allocator.createGroups(orders, pageSize)

	return allocator
}

// allocate returns the pool size charged for a request and advances libsmall's
// waste counters. A group starts with only its largest pool active; an optimal
// pool becomes active after its accumulated routing waste reaches a quarter of
// the group's slab size (src/lib/small/small/small.c:39-101,323-374).
func (a *smallAllocator) allocate(request uint32) uint32 {
	if request > a.objectMax {
		return request
	}

	optimalIndex := a.classes.classOffset(request)
	optimal := &a.pools[optimalIndex]
	group := &a.groups[optimal.group]
	appropriateMask := ^uint32(0) << optimal.groupOffset
	usedOffset := bits.TrailingZeros32(group.activeMask & appropriateMask)
	usedIndex := uint32(group.first) + uint32(usedOffset) //nolint:gosec // A group has at most 32 pools.
	usedSize := a.pools[usedIndex].objectSize

	if usedIndex != optimalIndex {
		optimal.waste += uint64(usedSize - optimal.objectSize)
		if optimal.waste >= group.wasteMax {
			group.activeMask |= uint32(1) << optimal.groupOffset
		}
	}

	return usedSize
}

// release mirrors smfree's waste accounting. Pool activation is irreversible
// during normal operation, but freeing an object routed through a larger pool
// subtracts that object's excess bytes from its optimal pool's waste counter
// (src/lib/small/small/small.c:379-405).
func (a *smallAllocator) release(request, charged uint32, count uint64) {
	if request > a.objectMax || count == 0 {
		return
	}

	optimal := &a.pools[a.classes.classOffset(request)]
	if charged <= optimal.objectSize {
		return
	}

	releasedWaste := uint64(charged-optimal.objectSize) * count
	if releasedWaste > optimal.waste {
		panic("memsize: allocator release exceeds routed waste")
	}

	optimal.waste -= releasedWaste
}

func (a *smallAllocator) createGroups(orders []uint8, pageSize uint32) {
	for first := 0; first < len(a.pools); {
		lastWithOrder := first
		for lastWithOrder+1 < len(a.pools) && orders[lastWithOrder+1] == orders[first] {
			lastWithOrder++
		}

		for groupFirst := first; groupFirst <= lastWithOrder; groupFirst += smallPoolsPerGroupMax {
			groupLast := min(groupFirst+smallPoolsPerGroupMax-1, lastWithOrder)
			groupIndex := len(a.groups)
			groupSize := groupLast - groupFirst + 1

			a.groups = append(a.groups, smallPoolGroup{
				first:      uint16(groupFirst),
				activeMask: uint32(1) << (groupSize - 1),
				wasteMax:   slabOrderSize(orders[groupLast], pageSize) / smallWasteDivisor,
			})

			for poolIndex := groupFirst; poolIndex <= groupLast; poolIndex++ {
				a.pools[poolIndex].group = uint16(groupIndex)                  //nolint:gosec // At most 1024 groups.
				a.pools[poolIndex].groupOffset = uint8(poolIndex - groupFirst) //nolint:gosec // Groups contain at most 32 pools.
			}
		}

		first = lastWithOrder + 1
	}
}

// mempoolSlabOrder follows mempool_create() and slab_order(). Pools target at
// most one-percent mslab overhead and use the host page size as order zero
// (src/lib/small/include/small/mempool.h:240-258 and small/slab_cache.c:169-193).
func mempoolSlabOrder(objectSize, pageSize uint32) uint8 {
	overhead := max(uint64(objectSize), smallMslabSize)
	desired := uint64(float64(overhead) / mempoolOverheadRatio)
	desired = min(desired, arenaSlabSize)

	orderMax := min(bits.Len64(arenaSlabSize/uint64(pageSize))-1, slabCacheOrderMax)

	orderZeroSize := arenaSlabSize >> orderMax
	if desired <= orderZeroSize {
		return 0
	}

	return uint8(bits.Len64(desired-1) - (bits.Len64(orderZeroSize) - 1)) //nolint:gosec // The result is bounded by slabCacheOrderMax.
}

func slabOrderSize(order uint8, pageSize uint32) uint64 {
	orderMax := min(bits.Len64(arenaSlabSize/uint64(pageSize))-1, slabCacheOrderMax)
	orderZeroSize := arenaSlabSize >> orderMax

	return orderZeroSize << order
}

func alignUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}
