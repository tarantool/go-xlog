package memsize

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrimaryKeyScratchNoAlloc(t *testing.T) { //nolint:paralleltest // testing.AllocsPerRun rejects parallel tests.
	pk := &Index{Parts: []Part{{FieldNo: 0, Type: "number"}}}
	tuple := []byte{0x92, 0xcf, 0, 0, 0, 0, 0, 0, 0, 1, 0xa3, 'o', 'n', 'e'}

	var scratch primaryKeyScratch

	want, err := scratch.tupleHash(tuple, pk)
	require.NoError(t, err)

	allocs := testing.AllocsPerRun(1000, func() {
		got, hashErr := scratch.tupleHash(tuple, pk)
		if hashErr != nil || got != want {
			panic("reused primary-key scratch changed the result")
		}
	})

	assert.Zero(t, allocs)
}
