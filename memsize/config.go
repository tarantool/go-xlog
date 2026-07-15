package memsize

import (
	"errors"
	"fmt"
	"math"
)

const (
	defaultMemtxMemory      = uint64(256 << 20)
	minimumMemtxMemory      = uint64(64 << 20)
	defaultAllocFactor      = 1.05
	defaultGranularity      = uint(8)
	minimumAllocation       = uint(16)
	maximumModeledTupleSize = 1 << 20
	maximumSmallClassValue  = uint64(1<<32 - 1)
)

// Config contains the box.cfg inputs that affect recovered memtx memory.
type Config struct {
	MemtxMemory uint64
	AllocFactor float64
	Granularity uint
	Allocator   Allocator

	FuncIndexKeysPerTuple map[string]float64
}

// Allocator selects Tarantool's memtx tuple allocator.
type Allocator int

const (
	// AllocatorSmall models Tarantool's default libsmall allocator.
	AllocatorSmall Allocator = iota
	// AllocatorSystem models malloc requests without size-class rounding.
	AllocatorSystem
)

// Configuration errors are comparable sentinels for errors.Is.
var (
	ErrInvalidAllocator   = errors.New("invalid memtx allocator")
	ErrInvalidAllocFactor = errors.New("invalid slab allocation factor")
	ErrInvalidGranularity = errors.New("invalid slab allocation granularity")
	ErrInvalidFuncKeys    = errors.New("invalid functional-index cardinality")
)

type normalizedConfig struct {
	Config

	budget          uint64
	defaultedBudget bool
	sizeClass       SizeClass
}

func normalizeConfig(cfg Config) (normalizedConfig, error) {
	normalized := normalizedConfig{Config: cfg}

	if normalized.MemtxMemory == 0 {
		normalized.MemtxMemory = defaultMemtxMemory
		normalized.defaultedBudget = true
	}

	if normalized.AllocFactor == 0 {
		normalized.AllocFactor = defaultAllocFactor
	}

	if normalized.Granularity == 0 {
		normalized.Granularity = defaultGranularity
	}

	switch normalized.Allocator {
	case AllocatorSmall, AllocatorSystem:
	default:
		return normalizedConfig{}, fmt.Errorf("memsize: config allocator %d: %w",
			normalized.Allocator, ErrInvalidAllocator)
	}

	if normalized.AllocFactor <= 1 || normalized.AllocFactor > 2 ||
		math.IsNaN(normalized.AllocFactor) || math.IsInf(normalized.AllocFactor, 0) {
		return normalizedConfig{}, fmt.Errorf("memsize: config alloc factor %v: %w",
			normalized.AllocFactor, ErrInvalidAllocFactor)
	}

	if normalized.Granularity == 0 ||
		normalized.Granularity&(normalized.Granularity-1) != 0 ||
		uint64(normalized.Granularity) > maximumSmallClassValue {
		return normalizedConfig{}, fmt.Errorf("memsize: config granularity %d: %w",
			normalized.Granularity, ErrInvalidGranularity)
	}

	for name, keys := range normalized.FuncIndexKeysPerTuple {
		if keys < 0 || math.IsNaN(keys) || math.IsInf(keys, 0) {
			return normalizedConfig{}, fmt.Errorf("memsize: config functional index %q=%v: %w",
				name, keys, ErrInvalidFuncKeys)
		}
	}

	normalized.budget = max(normalized.MemtxMemory, minimumMemtxMemory)
	minAlloc := max(minimumAllocation, normalized.Granularity)
	normalized.sizeClass = NewSizeClass(normalized.Granularity, normalized.AllocFactor, minAlloc)

	return normalized, nil
}
