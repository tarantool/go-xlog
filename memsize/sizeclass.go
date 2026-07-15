// Package memsize estimates the recovered memtx arena footprint of Tarantool
// snapshot and write-ahead log files.
package memsize

import (
	"math"
	"math/bits"
)

const smallClassUintBits = 32

// SizeClass rounds allocation requests to libsmall's small allocator classes.
// Its integer layout follows include/small/small_class.h:162-212.
type SizeClass struct {
	ignoreBits       int
	effectiveBits    int
	effectiveSize    uint32
	effectiveMask    uint32
	sizeShift        uint32
	sizeShiftPlusOne uint32
}

// NewSizeClass builds the class evaluator used by Tarantool's small allocator.
// The factor-to-effective-bits conversion follows small/small_class.c:37-58.
// Invalid inputs panic, matching the assertions in the C implementation.
func NewSizeClass(granularity uint, factor float64, minAlloc uint) SizeClass {
	if granularity == 0 || granularity&(granularity-1) != 0 {
		panic("memsize: size class granularity must be a power of two")
	}

	if factor <= 1 || factor > 2 || math.IsNaN(factor) {
		panic("memsize: size class factor must be in (1, 2]")
	}

	if minAlloc == 0 || minAlloc < granularity {
		panic("memsize: size class minimum allocation must be at least the granularity")
	}

	if granularity > math.MaxUint32 || minAlloc > math.MaxUint32 {
		panic("memsize: size class parameters exceed libsmall unsigned range")
	}

	effectiveBits := int(math.Round(math.Log(math.Ln2/math.Log(factor)) / math.Ln2))
	if effectiveBits >= smallClassUintBits {
		panic("memsize: size class factor is too close to one")
	}

	granularity32 := uint32(granularity)
	sizeShift := uint32(minAlloc) - granularity32
	effectiveSize := uint32(1) << effectiveBits

	return SizeClass{
		ignoreBits:       bits.TrailingZeros32(granularity32),
		effectiveBits:    effectiveBits,
		effectiveSize:    effectiveSize,
		effectiveMask:    effectiveSize - 1,
		sizeShift:        sizeShift,
		sizeShiftPlusOne: sizeShift + 1,
	}
}

// Round returns the smallest size-class allocation that can hold size bytes.
// Requests smaller than the configured minimum, including zero, round to the
// minimum class just as small_class_calc_offset_by_size does.
func (s SizeClass) Round(size uint32) uint32 {
	return s.classSize(s.classOffset(size))
}

func (s SizeClass) classOffset(size uint32) uint32 {
	checkedSize := size - s.sizeShiftPlusOne
	if checkedSize > size {
		size = 0
	} else {
		size = checkedSize
	}

	size >>= s.ignoreBits
	if size < s.effectiveSize {
		return size
	}

	log2 := bits.Len32(size>>s.effectiveBits) - 1
	linearPart := size >> log2
	log2Part := uint32(log2) << s.effectiveBits //nolint:gosec // bits.Len32 bounds log2 to [0, 31].

	return linearPart + log2Part
}

func (s SizeClass) classSize(class uint32) uint32 {
	class++
	linearPart := class & s.effectiveMask

	log2 := class >> s.effectiveBits
	if log2 != 0 {
		log2--
		linearPart |= s.effectiveSize
	}

	return s.sizeShift + (linearPart << log2 << s.ignoreBits)
}
