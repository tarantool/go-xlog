package memsize_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tarantool/go-xlog/memsize"
)

func TestTupleAllocSize(t *testing.T) {
	t.Parallel()

	defaultClasses := memsize.NewSizeClass(8, 1.05, 16)
	fineClasses := memsize.NewSizeClass(1, 1.0001, 1)

	tests := []struct {
		name         string
		fieldMapSize int
		bsize        int
		classes      memsize.SizeClass
		want         int
	}{
		{name: "minimum class", fieldMapSize: 0, bsize: 1, classes: defaultClasses, want: 16},
		{name: "default class rounding", fieldMapSize: 0, bsize: 100, classes: defaultClasses, want: 112},
		{name: "compact body boundary", fieldMapSize: 0, bsize: 255, classes: fineClasses, want: 265},
		{name: "bulky body boundary", fieldMapSize: 0, bsize: 256, classes: fineClasses, want: 270},
		{name: "compact offset boundary", fieldMapSize: 117, bsize: 130, classes: fineClasses, want: 257},
		{name: "bulky offset boundary", fieldMapSize: 118, bsize: 130, classes: fineClasses, want: 262},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, memsize.TupleAllocSize(test.fieldMapSize, test.bsize, test.classes))
		})
	}
}

func TestTreeIndexBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries int
		hinted  bool
		want    uint64
	}{
		{name: "empty", entries: 0, hinted: true, want: 0},
		{name: "three extent floor hinted", entries: 1, hinted: true, want: 48 << 10},
		{name: "four extents unhinted", entries: 3000, hinted: false, want: 64 << 10},
		{name: "six extents hinted", entries: 3000, hinted: true, want: 96 << 10},
		{name: "thirteen extents", entries: 10000, hinted: true, want: 208 << 10},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, memsize.TreeIndexBytes(test.entries, test.hinted))
		})
	}
}

func TestHashIndexBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries int
		want    uint64
	}{
		{name: "empty", entries: 0, want: 0},
		{name: "three extent floor", entries: 1, want: 48 << 10},
		{name: "three data extents", entries: 3072, want: 80 << 10},
		{name: "linear growth into fourth data extent", entries: 3073, want: 96 << 10},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, memsize.HashIndexBytes(test.entries))
		})
	}
}
