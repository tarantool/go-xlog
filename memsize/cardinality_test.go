package memsize //nolint:testpackage // Cardinality metadata and churn accumulation are deliberately internal.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

func TestIndexEntryCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		tuple any
		index Index
		want  uint32
	}{
		{
			name:  "array field",
			tuple: []any{uint64(1), []any{"a", "b", "c"}},
			index: Index{Multikey: true, Parts: []Part{{FieldNo: 1, path: "[*]"}}},
			want:  3,
		},
		{
			name: "nested map",
			tuple: []any{uint64(1), map[string]any{
				"items": []any{map[string]any{"value": 10}, map[string]any{"value": 20}},
			}},
			index: Index{Multikey: true, Parts: []Part{{FieldNo: 1, path: ".items[*].value"}}},
			want:  2,
		},
		{
			name: "quoted map keys",
			tuple: []any{uint64(1), map[string]any{
				"line-items": []any{map[string]any{"unit-price": 10}, map[string]any{"unit-price": 20}},
			}},
			index: Index{Multikey: true, Parts: []Part{{
				FieldNo: 1,
				path:    "['line-items'][*]['unit-price']",
			}}},
			want: 2,
		},
		{
			name:  "one based array path",
			tuple: []any{uint64(1), []any{[]any{10, 20, 30}, []any{40}}},
			index: Index{Multikey: true, Parts: []Part{{FieldNo: 1, path: "[1][*]"}}},
			want:  3,
		},
		{
			name:  "missing root",
			tuple: []any{uint64(1), map[string]any{}},
			index: Index{Multikey: true, Parts: []Part{{FieldNo: 1, path: ".items[*]"}}},
		},
		{
			name:  "nil root",
			tuple: []any{uint64(1), map[string]any{"items": nil}},
			index: Index{Multikey: true, Parts: []Part{{FieldNo: 1, path: ".items[*]"}}},
		},
		{
			name: "multikey exclude null",
			tuple: []any{uint64(1), []any{
				map[string]any{"value": 10},
				map[string]any{"value": nil},
				map[string]any{},
				map[string]any{"value": 40},
			}},
			index: Index{
				Multikey:    true,
				ExcludeNull: true,
				Parts: []Part{{
					FieldNo:     1,
					path:        "[*].value",
					excludeNull: true,
				}},
			},
			want: 2,
		},
		{
			name: "only flagged composite part excludes",
			tuple: []any{uint64(1), []any{
				map[string]any{"key": 10},
				map[string]any{"label": "kept despite missing unflagged key", "key": 20},
				map[string]any{"key": nil},
			}},
			index: Index{
				Multikey:    true,
				ExcludeNull: true,
				Parts: []Part{
					{FieldNo: 1, path: "[*].label"},
					{FieldNo: 1, path: "[*].key", excludeNull: true},
				},
			},
			want: 2,
		},
		{
			name:  "ordinary exclude null present",
			tuple: []any{uint64(1), map[string]any{"profile": map[string]any{"name": "Ada"}}},
			index: Index{
				ExcludeNull: true,
				Parts: []Part{{
					FieldNo:     1,
					path:        ".profile.name",
					excludeNull: true,
				}},
			},
			want: 1,
		},
		{
			name:  "ordinary exclude null missing",
			tuple: []any{uint64(1), map[string]any{"profile": map[string]any{}}},
			index: Index{
				ExcludeNull: true,
				Parts: []Part{{
					FieldNo:     1,
					path:        ".profile.name",
					excludeNull: true,
				}},
			},
		},
		{
			name:  "ordinary exclude null nil",
			tuple: []any{uint64(1), nil},
			index: Index{
				ExcludeNull: true,
				Parts:       []Part{{FieldNo: 1, excludeNull: true}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			raw := mustMarshalCardinality(t, test.tuple)
			got, err := indexEntryCount(raw, &test.index)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestIndexEntryCountRejectsInvalidPath(t *testing.T) {
	t.Parallel()

	tuple := mustMarshalCardinality(t, []any{uint64(1), []any{10}})
	index := Index{Multikey: true, Parts: []Part{{FieldNo: 1, path: "[0][*]"}}}

	_, err := indexEntryCount(tuple, &index)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidIndexPath)
}

func TestPathHasWildcardUsesTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
		err  bool
	}{
		{name: "wildcard", path: ".items[*].value", want: true},
		{name: "quoted wildcard text", path: "['[*]']", want: false},
		{name: "ordinary numeric subscript", path: "[1].items", want: false},
		{name: "two wildcards", path: "[*].items[*]", err: true},
		{name: "zero index", path: "[0]", err: true},
		{name: "missing separator", path: "items[1]value", err: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := pathHasWildcard(test.path)
			if test.err {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidIndexPath)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestDecodeIndexPartRetainsExcludeNull(t *testing.T) {
	t.Parallel()

	raw := mustMarshalCardinality(t, map[string]any{
		"field":        uint64(1),
		"type":         "unsigned",
		"path":         "[*].value",
		"exclude_null": true,
	})

	part, excludeNull, err := decodeMapIndexPart(raw)
	require.NoError(t, err)
	assert.True(t, excludeNull)
	assert.True(t, part.excludeNull)
}

func TestIndexEntryCountDoesNotAllocate(t *testing.T) { //nolint:paralleltest // testing.AllocsPerRun rejects parallel tests.
	tuple := mustMarshalCardinality(t, []any{uint64(1), []any{
		map[string]any{"value": 10},
		map[string]any{"value": nil},
		map[string]any{"value": 30},
	}})
	index := Index{
		Multikey:    true,
		ExcludeNull: true,
		Parts: []Part{{
			FieldNo:        1,
			path:           "[*].value",
			excludeNull:    true,
			wildcard:       true,
			wildcardSuffix: ".value",
		}},
	}

	allocations := testing.AllocsPerRun(1000, func() {
		entries, err := indexEntryCount(tuple, &index)
		if err != nil {
			panic(err)
		}

		if entries != 2 {
			panic("unexpected entry count")
		}
	})

	assert.Zero(t, allocations)
}

func TestAccumulatorTracksVariableIndexEntries(t *testing.T) {
	t.Parallel()

	const spaceID = uint32(600)

	space := &Space{
		ID:     spaceID,
		Name:   "variable_entries",
		Engine: "memtx",
		Kind:   SpaceNormal,
		Indexes: []Index{
			{ID: 0, Name: "pk", Type: IndexTree, Unique: true, Hinted: true, Parts: []Part{{FieldNo: 0}}},
			{
				ID:          1,
				Name:        "by_value",
				Type:        IndexTree,
				Hinted:      true,
				Multikey:    true,
				ExcludeNull: true,
				Parts: []Part{{
					FieldNo:     1,
					path:        "[*].value",
					excludeNull: true,
				}},
			},
			{
				ID:          2,
				Name:        "by_optional",
				Type:        IndexTree,
				Hinted:      true,
				ExcludeNull: true,
				Parts:       []Part{{FieldNo: 2, excludeNull: true}},
			},
		},
	}
	schema := &Schema{Spaces: map[uint32]*Space{spaceID: space}}

	cfg, err := normalizeConfig(Config{MemtxMemory: 64 << 20})
	require.NoError(t, err)

	accumulator := newSnapshotAccumulator(schema, cfg)
	firstTuple := mustMarshalCardinality(t, []any{uint64(1), []any{
		map[string]any{"value": 10},
		map[string]any{"value": nil},
		map[string]any{},
	}, "present"})
	secondTuple := mustMarshalCardinality(t, []any{uint64(2), []any{
		map[string]any{"value": 20},
		map[string]any{"value": 30},
	}, nil})

	first, added, err := accumulator.addTuple(spaceID, firstTuple)
	require.NoError(t, err)
	require.True(t, added)

	first = first.retain()

	second, added, err := accumulator.addTuple(spaceID, secondTuple)
	require.NoError(t, err)
	require.True(t, added)

	second = second.retain()

	report := accumulator.buildReport(SourceInfo{})
	require.Len(t, report.Spaces, 1)
	assert.Equal(t, []uint64{2, 3, 1}, reportIndexEntries(report.Spaces[0]))
	assert.Equal(t, BoundLower, report.Spaces[0].Bound)
	assert.True(t, reportHasWarning(report, WarnMultikeyTupleAllocation, space.Name))

	for _, index := range report.Spaces[0].Indexes {
		assert.False(t, index.Estimated, index.Name)
	}

	replacementTuple := mustMarshalCardinality(t, []any{
		uint64(1),
		[]any{map[string]any{"value": nil}},
		nil,
	})
	replacement, allocated, err := accumulator.allocateTuple(spaceID, replacementTuple)
	require.NoError(t, err)
	require.True(t, allocated)
	accumulator.replaceTuple(spaceID, first, replacement)

	report = accumulator.buildReport(SourceInfo{})
	assert.Equal(t, []uint64{2, 2, 0}, reportIndexEntries(report.Spaces[0]))
	assert.True(t, report.Spaces[0].Indexes[1].Estimated, "TREE layout drifts after replacement churn")

	accumulator.removeTuple(spaceID, second)
	report = accumulator.buildReport(SourceInfo{})
	assert.Equal(t, []uint64{1, 0, 0}, reportIndexEntries(report.Spaces[0]))

	accumulator.clearSpace(spaceID)
	report = accumulator.buildReport(SourceInfo{})
	assert.Equal(t, []uint64{0, 0, 0}, reportIndexEntries(report.Spaces[0]))
}

func reportIndexEntries(space SpaceReport) []uint64 {
	entries := make([]uint64, len(space.Indexes))
	for i := range space.Indexes {
		entries[i] = space.Indexes[i].Entries
	}

	return entries
}

func reportHasWarning(report *Report, kind WarningKind, space string) bool {
	for _, warning := range report.Warnings {
		if warning.Kind == kind && warning.Space == space {
			return true
		}
	}

	return false
}

func mustMarshalCardinality(t *testing.T, value any) []byte {
	t.Helper()

	raw, err := msgpack.Marshal(value)
	require.NoError(t, err)

	return raw
}
