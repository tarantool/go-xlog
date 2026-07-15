package memsize_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/memsize"
	"github.com/tarantool/go-xlog/reader"
)

const (
	spaceSchemaID = 280
	indexSchemaID = 288
)

func TestBuildSchemaHistorical211(t *testing.T) {
	t.Parallel()

	schema := schemaFromSnapshot(t, "historical/2.11/00000000000000000022.snap")

	testSpace, ok := schema.Space(512)
	require.True(t, ok)
	assert.Equal(t, "test", testSpace.Name)
	assert.Equal(t, "memtx", testSpace.Engine)
	assert.Equal(t, memsize.SpaceNormal, testSpace.Kind)
	assert.True(t, testSpace.IsMemtxData())
	assert.Zero(t, testSpace.FieldMapSize)
	require.NotNil(t, testSpace.PK())
	assert.Equal(t, memsize.IndexTree, testSpace.PK().Type)
	assert.True(t, testSpace.PK().Unique)
	assert.True(t, testSpace.PK().Hinted)
	assert.Equal(t, []memsize.Part{{FieldNo: 0, Type: "unsigned"}}, testSpace.PK().Parts)

	vinylSpace, ok := schema.Space(514)
	require.True(t, ok)
	assert.Equal(t, "vinyl", vinylSpace.Engine)
	assert.False(t, vinylSpace.IsMemtxData())

	indexSpace, ok := schema.Space(288)
	require.True(t, ok)
	assert.Equal(t, 4, indexSpace.FieldMapSize, "golden indexes [0,1] and [0,2] need only field 2")
	require.Len(t, indexSpace.Indexes, 2)
	assert.Equal(t, uint32(0), indexSpace.Indexes[0].ID)
	assert.Equal(t, uint32(2), indexSpace.Indexes[1].ID)

	privSpace, ok := schema.Space(312)
	require.True(t, ok)
	assert.Equal(t, 12, privSpace.FieldMapSize, "golden primary key [1,2,3] needs three slots")

	localSpace, ok := schema.Space(257)
	require.True(t, ok)
	assert.Equal(t, uint32(1), localSpace.GroupID)
	assert.Equal(t, memsize.SpaceLocal, localSpace.Kind)

	temporarySpace, ok := schema.Space(380)
	require.True(t, ok)
	assert.Equal(t, memsize.SpaceDataTemporary, temporarySpace.Kind)
	assert.False(t, temporarySpace.IsMemtxData())
}

func TestSchemaCurrentOptionsAndParts(t *testing.T) {
	t.Parallel()

	schema := memsize.BuildSchema()
	spaceTuple := mustMsgpack(t, []any{
		uint64(600), uint64(1), "features", "memtx", uint64(0),
		map[string]any{"group_id": uint64(1)},
		[]any{
			map[string]any{"name": "id", "type": "unsigned"},
			map[string]any{"name": "items", "type": "array"},
			map[string]any{"name": "body", "type": "string", "compression": "zstd"},
		},
	})
	require.NoError(t, schema.ApplySpaceRow(iproto.IPROTO_INSERT, spaceTuple))

	indexes := []any{
		[]any{
			uint64(600), uint64(0), "pk", "tree", map[string]any{"unique": true},
			[]any{
				map[string]any{"field": uint64(0), "type": "unsigned"},
				map[string]any{"field": uint64(1), "type": "array"},
			},
		},
		[]any{
			uint64(600), uint64(1), "by_name", "tree",
			map[string]any{"unique": false, "hint": false},
			[]any{map[string]any{
				"field": uint64(0), "type": "string", "path": ".profile.name",
				"is_nullable": true, "collation": uint64(2), "exclude_null": true,
			}},
		},
		[]any{
			uint64(600), uint64(2), "functional", "tree",
			map[string]any{"func": uint64(42), "hint": false},
			[]any{map[string]any{"field": uint64(0), "type": "unsigned"}},
		},
		[]any{
			uint64(600), uint64(3), "multikey", "tree",
			map[string]any{"unique": false, "hint": false},
			[]any{map[string]any{
				"field": uint64(1), "type": "string", "path": ".items[*].name",
			}},
		},
	}

	for _, indexTuple := range indexes {
		require.NoError(t, schema.ApplyIndexRow(iproto.IPROTO_INSERT, mustMsgpack(t, indexTuple)))
	}

	space, ok := schema.Space(600)
	require.True(t, ok)
	assert.Equal(t, memsize.SpaceLocal, space.Kind)
	assert.True(t, space.Compressed)
	assert.Equal(t, 12, space.FieldMapSize, "one JSON leaf plus multikey root and leaf")
	require.Len(t, space.Indexes, 4)

	byName := space.Indexes[1]
	assert.False(t, byName.Unique)
	assert.False(t, byName.Hinted)
	assert.True(t, byName.ExcludeNull)
	require.Len(t, byName.Parts, 1)
	assert.Equal(t, "2", byName.Parts[0].Collation)
	assert.True(t, byName.Parts[0].Nullable)

	functional := space.Indexes[2]
	assert.True(t, functional.Functional, "opts.func presence marks a functional index")
	assert.True(t, functional.Hinted, "functional indexes force hints")

	multikey := space.Indexes[3]
	assert.True(t, multikey.Multikey)
	assert.True(t, multikey.Hinted, "multikey indexes force hints")
}

func TestSchemaSpaceKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		engine string
		opts   map[string]any
		want   memsize.SpaceKind
		data   bool
	}{
		{name: "normal", engine: "memtx", opts: map[string]any{}, want: memsize.SpaceNormal, data: true},
		{name: "local", engine: "memtx", opts: map[string]any{"group_id": uint64(1)}, want: memsize.SpaceLocal, data: true},
		{name: "data temporary", engine: "memtx", opts: map[string]any{"type": "data-temporary"}, want: memsize.SpaceDataTemporary},
		{name: "legacy temporary", engine: "memtx", opts: map[string]any{"temporary": true}, want: memsize.SpaceDataTemporary},
		{name: "temporary", engine: "memtx", opts: map[string]any{"type": "temporary"}, want: memsize.SpaceTemporary},
		{name: "view", engine: "memtx", opts: map[string]any{"view": true}, want: memsize.SpaceView},
		{name: "is_view alias", engine: "memtx", opts: map[string]any{"is_view": true}, want: memsize.SpaceView},
		{name: "sysview", engine: "sysview", opts: map[string]any{}, want: memsize.SpaceSysView},
		{name: "vinyl", engine: "vinyl", opts: map[string]any{}, want: memsize.SpaceNormal},
	}

	for id, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := memsize.BuildSchema()
			tuple := []any{uint64(700 + id), uint64(1), test.name, test.engine, uint64(0), test.opts, []any{}}
			require.NoError(t, schema.ApplySpaceRow(iproto.IPROTO_INSERT, mustMsgpack(t, tuple)))

			space, ok := schema.Space(uint32(700 + id))
			require.True(t, ok)
			assert.Equal(t, test.want, space.Kind)
			assert.Equal(t, test.data, space.IsMemtxData())
		})
	}
}

func TestSchemaApplyReplaceAndDelete(t *testing.T) {
	t.Parallel()

	schema := memsize.BuildSchema()
	spaceTuple := []any{uint64(800), uint64(1), "before", "memtx", uint64(0), map[string]any{}, []any{}}
	require.NoError(t, schema.ApplySpaceRow(iproto.IPROTO_INSERT, mustMsgpack(t, spaceTuple)))

	indexTuple := []any{
		uint64(800), uint64(0), "pk", "hash", map[string]any{"unique": true},
		[]any{[]any{uint64(0), "unsigned"}},
	}
	require.NoError(t, schema.ApplyIndexRow(iproto.IPROTO_INSERT, mustMsgpack(t, indexTuple)))

	spaceTuple[2] = "after"
	require.NoError(t, schema.ApplySpaceRow(iproto.IPROTO_REPLACE, mustMsgpack(t, spaceTuple)))
	space, ok := schema.Space(800)
	require.True(t, ok)
	assert.Equal(t, "after", space.Name)
	require.NotNil(t, space.PK(), "space replacement must preserve its indexes")
	assert.Equal(t, memsize.IndexHash, space.PK().Type)

	require.NoError(t, schema.ApplyIndexRow(iproto.IPROTO_DELETE, mustMsgpack(t, []any{uint64(800), uint64(0)})))
	assert.Nil(t, space.PK())

	require.NoError(t, schema.ApplySpaceRow(iproto.IPROTO_DELETE, mustMsgpack(t, []any{uint64(800)})))
	_, ok = schema.Space(800)
	assert.False(t, ok)
}

func TestSchemaApplyTruncateRows(t *testing.T) {
	t.Parallel()

	schema := memsize.BuildSchema()
	tuple := mustMsgpack(t, []any{uint64(512)})

	for _, typ := range []iproto.Type{
		iproto.IPROTO_INSERT,
		iproto.IPROTO_REPLACE,
		iproto.IPROTO_UPDATE,
		iproto.IPROTO_UPSERT,
		iproto.IPROTO_DELETE,
	} {
		require.NoError(t, schema.ApplyTruncateRow(typ, tuple), typ)
	}

	err := schema.ApplyTruncateRow(iproto.IPROTO_NOP, tuple)
	require.Error(t, err)
	assert.ErrorIs(t, err, memsize.ErrUnsupportedSchemaOp)
}

func schemaFromSnapshot(t *testing.T, name string) *memsize.Schema {
	t.Helper()

	r, err := reader.Open(testutil.Path(t, name), reader.WithAliasBodies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })

	schema := memsize.BuildSchema()

	for r.Scan() {
		row := r.Row()
		if row.Type != iproto.IPROTO_INSERT {
			continue
		}

		body, err := format.DecodeDMLBody(row.BodyRaw)
		require.NoError(t, err)

		switch body.SpaceID {
		case spaceSchemaID:
			require.NoError(t, schema.ApplySpaceRow(row.Type, body.Tuple))
		case indexSchemaID:
			require.NoError(t, schema.ApplyIndexRow(row.Type, body.Tuple))
		}
	}

	require.NoError(t, r.Err())

	return schema
}

func mustMsgpack(t *testing.T, value any) []byte {
	t.Helper()

	b, err := msgpack.Marshal(value)
	require.NoError(t, err)

	return b
}
