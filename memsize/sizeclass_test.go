package memsize_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tarantool/go-xlog/memsize"
)

func TestSizeClassDefaultTable(t *testing.T) {
	t.Parallel()

	// These are the first five groups emitted by libsmall's small_class for
	// (granularity=8, factor=1.05, min_alloc=16). The first group is linear;
	// each following group has 16 entries before its increment doubles.
	classSizes := []uint32{
		16, 24, 32, 40, 48, 56, 64, 72,
		80, 88, 96, 104, 112, 120, 128, 136,
		144, 152, 160, 168, 176, 184, 192, 200,
		208, 216, 224, 232, 240, 248, 256, 264,
		280, 296, 312, 328, 344, 360, 376, 392,
		408, 424, 440, 456, 472, 488, 504, 520,
		552, 584, 616, 648, 680, 712, 744, 776,
		808, 840, 872, 904, 936, 968, 1000, 1032,
		1096, 1160, 1224, 1288, 1352, 1416, 1480, 1544,
		1608, 1672, 1736, 1800, 1864, 1928, 1992, 2056,
	}

	sc := memsize.NewSizeClass(8, 1.05, 16)
	previous := uint32(0)

	for class, want := range classSizes {
		assert.Equal(t, want, sc.Round(previous+1), "class %d lower boundary", class)
		assert.Equal(t, want, sc.Round(want), "class %d upper boundary", class)
		previous = want
	}

	assert.Equal(t, uint32(16), sc.Round(0))
}
