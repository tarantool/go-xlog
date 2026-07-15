package memsize //nolint:testpackage // The libsmall state machine is deliberately internal.

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSmallAllocatorInitialPoolSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request uint32
		want    uint32
	}{
		{name: "first class", request: 1, want: 128},
		{name: "second class", request: 65, want: 128},
		{name: "32 KiB slab", request: 129, want: 256},
		{name: "64 KiB slab", request: 257, want: 512},
		{name: "128 KiB slab", request: 513, want: 1024},
		{name: "256 KiB slab", request: 1025, want: 2048},
		{name: "512 KiB slab", request: 2049, want: 4096},
		{name: "1 MiB slab", request: 4097, want: 8192},
		{name: "2 MiB slab", request: 8193, want: 16384},
		{name: "4 MiB slab", request: 16385, want: 32768},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			classes := NewSizeClass(64, 1.5, 64)
			allocator := newSmallAllocator(classes, 16<<10)

			assert.Equal(t, test.want, allocator.allocate(test.request))
		})
	}
}

func TestSmallAllocatorActivatesOptimalPoolAtWasteLimit(t *testing.T) {
	t.Parallel()

	for _, pageSize := range []uint32{4 << 10, 8 << 10, 16 << 10} {
		t.Run(strconv.FormatUint(uint64(pageSize), 10), func(t *testing.T) {
			t.Parallel()

			classes := NewSizeClass(64, 1.5, 64)
			assert.Equal(t, uint32(3072), classes.Round(2240))

			allocator := newSmallAllocator(classes, pageSize)

			for i := range 200 {
				want := uint32(4096)
				if i > 127 {
					want = 3072
				}

				assert.Equal(t, want, allocator.allocate(2240), "allocation %d", i)
			}
		})
	}
}

func TestSmallAllocatorReleaseSubtractsRoutingWaste(t *testing.T) {
	t.Parallel()

	classes := NewSizeClass(64, 1.5, 64)
	allocator := newSmallAllocator(classes, 16<<10)

	for i := range 200 {
		charged := allocator.allocate(2240)
		assert.Equal(t, uint32(4096), charged, "allocation %d", i)
		allocator.release(2240, charged, 1)
	}
}
