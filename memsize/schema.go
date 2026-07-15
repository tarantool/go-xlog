package memsize

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

const (
	fieldMapSlotSize = 4

	spaceRowFieldCount  = 7
	indexRowFieldCount  = 6
	indexKeyFieldCount  = 2
	legacyPartMinFields = 2
)

// Schema decoding errors are comparable sentinels so callers can distinguish
// malformed rows from unsupported operations and missing dependencies.
var (
	ErrInvalidSpaceRow     = errors.New("invalid _space row")
	ErrInvalidIndexRow     = errors.New("invalid _index row")
	ErrInvalidTruncateRow  = errors.New("invalid _truncate row")
	ErrUnsupportedSchemaOp = errors.New("unsupported schema operation")
	ErrSpaceNotFound       = errors.New("schema space not found")
	ErrUnknownIndexType    = errors.New("unknown index type")
	ErrUnknownSpaceKind    = errors.New("unknown space kind")
)

// Schema is the subset of Tarantool's data dictionary needed to size memtx.
type Schema struct {
	Spaces map[uint32]*Space
}

// Space describes one row from Tarantool's _space system space.
type Space struct {
	ID         uint32
	Name       string
	Engine     string
	Kind       SpaceKind
	GroupID    uint32
	Compressed bool

	FieldMapSize int
	Indexes      []Index

	fieldNullable []bool
}

// Index describes one row from Tarantool's _index system space.
type Index struct {
	ID          uint32
	Name        string
	Type        IndexType
	Unique      bool
	Hinted      bool
	Multikey    bool
	Functional  bool
	ExcludeNull bool
	Parts       []Part
}

// Part describes one indexed tuple field.
type Part struct {
	FieldNo   uint32
	Type      string
	Collation string
	Nullable  bool

	path           string
	excludeNull    bool
	wildcard       bool
	wildcardSuffix string
}

// IndexType is a memtx index implementation.
type IndexType int

const (
	// IndexTree is Tarantool's BPS tree index.
	IndexTree IndexType = iota
	// IndexHash is Tarantool's linear hash index.
	IndexHash
	// IndexRTree is Tarantool's spatial R-tree index.
	IndexRTree
	// IndexBitset is Tarantool's bitset index.
	IndexBitset
)

// SpaceKind captures persistence and view semantics from _space options.
type SpaceKind int

const (
	// SpaceNormal is an ordinary persistent space.
	SpaceNormal SpaceKind = iota
	// SpaceDataTemporary persists schema but not tuples.
	SpaceDataTemporary
	// SpaceTemporary persists neither schema nor tuples.
	SpaceTemporary
	// SpaceLocal is a persistent replica-local space.
	SpaceLocal
	// SpaceView is a user-defined view.
	SpaceView
	// SpaceSysView is a system view backed by another space.
	SpaceSysView
)

// BuildSchema returns an empty mutable schema.
func BuildSchema() *Schema {
	return &Schema{Spaces: make(map[uint32]*Space)}
}

// ApplySpaceRow applies an INSERT, REPLACE, or DELETE row from _space (280).
func (s *Schema) ApplySpaceRow(op iproto.Type, tuple []byte) error {
	if s == nil {
		return fmt.Errorf("memsize: ApplySpaceRow: %w", ErrInvalidSpaceRow)
	}

	if s.Spaces == nil {
		s.Spaces = make(map[uint32]*Space)
	}

	switch op {
	case iproto.IPROTO_INSERT, iproto.IPROTO_REPLACE:
		space, err := decodeSpaceRow(tuple)
		if err != nil {
			return fmt.Errorf("memsize: ApplySpaceRow: %w", err)
		}

		if old := s.Spaces[space.ID]; old != nil {
			space.Indexes = old.Indexes
			recomputeFieldMapSize(space)
		}

		s.Spaces[space.ID] = space

		return nil
	case iproto.IPROTO_DELETE:
		id, err := decodeFirstUint32(tuple, ErrInvalidSpaceRow)
		if err != nil {
			return fmt.Errorf("memsize: ApplySpaceRow: %w", err)
		}

		delete(s.Spaces, id)

		return nil
	default:
		return fmt.Errorf("memsize: ApplySpaceRow: %w: %s", ErrUnsupportedSchemaOp, op)
	}
}

// ApplyIndexRow applies an INSERT, REPLACE, or DELETE row from _index (288).
func (s *Schema) ApplyIndexRow(op iproto.Type, tuple []byte) error {
	if s == nil {
		return fmt.Errorf("memsize: ApplyIndexRow: %w", ErrInvalidIndexRow)
	}

	switch op {
	case iproto.IPROTO_INSERT, iproto.IPROTO_REPLACE:
		spaceID, _, err := decodeIndexKey(tuple)
		if err != nil {
			return fmt.Errorf("memsize: ApplyIndexRow: %w", err)
		}

		space := s.Spaces[spaceID]
		if space == nil {
			return fmt.Errorf("memsize: ApplyIndexRow: space %d: %w", spaceID, ErrSpaceNotFound)
		}

		index, err := decodeIndexRow(tuple, space)
		if err != nil {
			return fmt.Errorf("memsize: ApplyIndexRow: %w", err)
		}

		pos, found := findIndex(space.Indexes, index.ID)
		if found {
			space.Indexes[pos] = index
		} else {
			space.Indexes = append(space.Indexes, index)
		}

		slices.SortFunc(space.Indexes, func(a, b Index) int {
			return int(a.ID) - int(b.ID)
		})
		recomputeFieldMapSize(space)

		return nil
	case iproto.IPROTO_DELETE:
		spaceID, indexID, err := decodeIndexKey(tuple)
		if err != nil {
			return fmt.Errorf("memsize: ApplyIndexRow: %w", err)
		}

		space := s.Spaces[spaceID]
		if space == nil {
			return fmt.Errorf("memsize: ApplyIndexRow: space %d: %w", spaceID, ErrSpaceNotFound)
		}

		pos, found := findIndex(space.Indexes, indexID)
		if found {
			space.Indexes = slices.Delete(space.Indexes, pos, pos+1)
			recomputeFieldMapSize(space)
		}

		return nil
	default:
		return fmt.Errorf("memsize: ApplyIndexRow: %w: %s", ErrUnsupportedSchemaOp, op)
	}
}

// ApplyTruncateRow validates a row from _truncate (330). Truncation changes
// tuple state rather than schema, so the replay accumulator performs the clear.
func (s *Schema) ApplyTruncateRow(op iproto.Type, tuple []byte) error {
	if s == nil {
		return fmt.Errorf("memsize: ApplyTruncateRow: %w", ErrInvalidTruncateRow)
	}

	switch op {
	case iproto.IPROTO_INSERT,
		iproto.IPROTO_REPLACE,
		iproto.IPROTO_UPDATE,
		iproto.IPROTO_UPSERT,
		iproto.IPROTO_DELETE:
		if _, err := decodeFirstUint32(tuple, ErrInvalidTruncateRow); err != nil {
			return fmt.Errorf("memsize: ApplyTruncateRow: %w", err)
		}

		return nil
	default:
		return fmt.Errorf("memsize: ApplyTruncateRow: %w: %s", ErrUnsupportedSchemaOp, op)
	}
}

// Space returns the space with id.
func (s *Schema) Space(id uint32) (*Space, bool) {
	if s == nil {
		return nil, false
	}

	space, ok := s.Spaces[id]

	return space, ok
}

// IsMemtxData reports whether persisted tuples from the space occupy memtx.
func (s *Space) IsMemtxData() bool {
	return s != nil && s.Engine == "memtx" && (s.Kind == SpaceNormal || s.Kind == SpaceLocal)
}

// PK returns the primary index, or nil if the space has none.
func (s *Space) PK() *Index {
	if s == nil {
		return nil
	}

	for i := range s.Indexes {
		if s.Indexes[i].ID == 0 {
			return &s.Indexes[i]
		}
	}

	return nil
}

func decodeSpaceRow(tuple []byte) (*Space, error) {
	cursor := format.NewMPCursor(tuple)

	fieldCount, err := cursor.ArrayLen()
	if err != nil {
		return nil, fmt.Errorf("%w: tuple: %w", ErrInvalidSpaceRow, err)
	}

	if fieldCount < spaceRowFieldCount {
		return nil, fmt.Errorf("%w: got %d fields, need at least %d", ErrInvalidSpaceRow, fieldCount, spaceRowFieldCount)
	}

	id, err := cursorUint32(&cursor, ErrInvalidSpaceRow, "id")
	if err != nil {
		return nil, err
	}

	if err := cursor.Skip(); err != nil {
		return nil, fmt.Errorf("%w: owner: %w", ErrInvalidSpaceRow, err)
	}

	name, err := cursor.Str()
	if err != nil {
		return nil, fmt.Errorf("%w: name: %w", ErrInvalidSpaceRow, err)
	}

	engine, err := cursor.Str()
	if err != nil {
		return nil, fmt.Errorf("%w: engine: %w", ErrInvalidSpaceRow, err)
	}

	if _, err := cursor.Uint(); err != nil {
		return nil, fmt.Errorf("%w: field_count: %w", ErrInvalidSpaceRow, err)
	}

	optsRaw, err := cursor.Raw()
	if err != nil {
		return nil, fmt.Errorf("%w: opts: %w", ErrInvalidSpaceRow, err)
	}

	formatRaw, err := cursor.Raw()
	if err != nil {
		return nil, fmt.Errorf("%w: format: %w", ErrInvalidSpaceRow, err)
	}

	for field := spaceRowFieldCount; field < fieldCount; field++ {
		if err := cursor.Skip(); err != nil {
			return nil, fmt.Errorf("%w: field %d: %w", ErrInvalidSpaceRow, field, err)
		}
	}

	opts, err := decodeSpaceOpts(optsRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: opts: %w", ErrInvalidSpaceRow, err)
	}

	nullable, compressed, err := decodeSpaceFormat(formatRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: format: %w", ErrInvalidSpaceRow, err)
	}

	space := &Space{
		ID:            id,
		Name:          string(name),
		Engine:        string(engine),
		Kind:          opts.kind,
		GroupID:       opts.groupID,
		Compressed:    compressed,
		fieldNullable: nullable,
	}
	space.Kind = resolveSpaceKind(space.Engine, opts)

	return space, nil
}

type spaceOpts struct {
	kind       SpaceKind
	groupID    uint32
	legacyTemp bool
	view       bool
}

func decodeSpaceOpts(raw []byte) (spaceOpts, error) {
	opts := spaceOpts{kind: SpaceNormal}
	cursor := format.NewMPCursor(raw)

	count, err := cursor.MapLen()
	if err != nil {
		return opts, fmt.Errorf("map: %w", err)
	}

	for entry := range count {
		key, err := cursor.Str()
		if err != nil {
			return opts, fmt.Errorf("key %d: %w", entry, err)
		}

		switch string(key) {
		case "group_id":
			opts.groupID, err = cursorUint32(&cursor, ErrInvalidSpaceRow, "group_id")
		case "type":
			var value []byte

			value, err = cursor.Str()
			if err == nil {
				switch string(value) {
				case "normal":
					opts.kind = SpaceNormal
				case "data-temporary":
					opts.kind = SpaceDataTemporary
				case "temporary":
					opts.kind = SpaceTemporary
				default:
					return opts, fmt.Errorf("%w: %q", ErrUnknownSpaceKind, value)
				}
			}
		case "temporary":
			opts.legacyTemp, err = cursor.Bool()
		case "view", "is_view":
			opts.view, err = cursor.Bool()
		default:
			err = cursor.Skip()
		}

		if err != nil {
			return opts, fmt.Errorf("option %q: %w", key, err)
		}
	}

	if opts.legacyTemp {
		opts.kind = SpaceDataTemporary
	}

	return opts, nil
}

func resolveSpaceKind(engine string, opts spaceOpts) SpaceKind {
	switch {
	case engine == "sysview":
		return SpaceSysView
	case opts.view:
		return SpaceView
	case opts.kind != SpaceNormal:
		return opts.kind
	case opts.groupID == 1:
		return SpaceLocal
	default:
		return SpaceNormal
	}
}

func decodeSpaceFormat(raw []byte) ([]bool, bool, error) {
	cursor := format.NewMPCursor(raw)

	count, err := cursor.ArrayLen()
	if err != nil {
		return nil, false, fmt.Errorf("array: %w", err)
	}

	nullable := make([]bool, count)
	compressed := false

	for field := range count {
		optionCount, err := cursor.MapLen()
		if err != nil {
			return nil, false, fmt.Errorf("field %d: %w", field, err)
		}

		for option := range optionCount {
			key, err := cursor.Str()
			if err != nil {
				return nil, false, fmt.Errorf("field %d option %d: %w", field, option, err)
			}

			switch string(key) {
			case "is_nullable":
				nullable[field], err = cursor.Bool()
			case "compression":
				compressed = true
				err = cursor.Skip()
			default:
				err = cursor.Skip()
			}

			if err != nil {
				return nil, false, fmt.Errorf("field %d option %q: %w", field, key, err)
			}
		}
	}

	return nullable, compressed, nil
}

func decodeIndexRow(tuple []byte, space *Space) (Index, error) {
	cursor := format.NewMPCursor(tuple)

	fieldCount, err := cursor.ArrayLen()
	if err != nil {
		return Index{}, fmt.Errorf("%w: tuple: %w", ErrInvalidIndexRow, err)
	}

	if fieldCount < indexRowFieldCount {
		return Index{}, fmt.Errorf("%w: got %d fields, need at least %d", ErrInvalidIndexRow, fieldCount, indexRowFieldCount)
	}

	spaceID, err := cursorUint32(&cursor, ErrInvalidIndexRow, "space id")
	if err != nil {
		return Index{}, err
	}

	if spaceID != space.ID {
		return Index{}, fmt.Errorf("%w: space id changed from %d to %d", ErrInvalidIndexRow, space.ID, spaceID)
	}

	id, err := cursorUint32(&cursor, ErrInvalidIndexRow, "index id")
	if err != nil {
		return Index{}, err
	}

	name, err := cursor.Str()
	if err != nil {
		return Index{}, fmt.Errorf("%w: name: %w", ErrInvalidIndexRow, err)
	}

	typeName, err := cursor.Str()
	if err != nil {
		return Index{}, fmt.Errorf("%w: type: %w", ErrInvalidIndexRow, err)
	}

	indexType, err := parseIndexType(string(typeName))
	if err != nil {
		return Index{}, err
	}

	optsRaw, err := cursor.Raw()
	if err != nil {
		return Index{}, fmt.Errorf("%w: opts: %w", ErrInvalidIndexRow, err)
	}

	partsRaw, err := cursor.Raw()
	if err != nil {
		return Index{}, fmt.Errorf("%w: parts: %w", ErrInvalidIndexRow, err)
	}

	for field := indexRowFieldCount; field < fieldCount; field++ {
		if err := cursor.Skip(); err != nil {
			return Index{}, fmt.Errorf("%w: field %d: %w", ErrInvalidIndexRow, field, err)
		}
	}

	index := Index{ID: id, Name: string(name), Type: indexType, Unique: true, Hinted: true}
	if err := decodeIndexOpts(optsRaw, &index); err != nil {
		return Index{}, fmt.Errorf("%w: opts: %w", ErrInvalidIndexRow, err)
	}

	parts, err := decodeIndexParts(partsRaw, space, &index)
	if err != nil {
		return Index{}, fmt.Errorf("%w: parts: %w", ErrInvalidIndexRow, err)
	}

	index.Parts = parts
	if index.Functional || index.Multikey {
		index.Hinted = true
	}

	return index, nil
}

func decodeIndexOpts(raw []byte, index *Index) error {
	cursor := format.NewMPCursor(raw)

	count, err := cursor.MapLen()
	if err != nil {
		return fmt.Errorf("map: %w", err)
	}

	for entry := range count {
		key, err := cursor.Str()
		if err != nil {
			return fmt.Errorf("key %d: %w", entry, err)
		}

		switch string(key) {
		case "unique":
			index.Unique, err = cursor.Bool()
		case "hint":
			index.Hinted, err = cursor.Bool()
		case "func":
			index.Functional = true
			err = cursor.Skip()
		case "exclude_null":
			index.ExcludeNull, err = cursor.Bool()
		default:
			err = cursor.Skip()
		}

		if err != nil {
			return fmt.Errorf("option %q: %w", key, err)
		}
	}

	return nil
}

func decodeIndexParts(raw []byte, space *Space, index *Index) ([]Part, error) {
	cursor := format.NewMPCursor(raw)

	count, err := cursor.ArrayLen()
	if err != nil {
		return nil, fmt.Errorf("array: %w", err)
	}

	parts := make([]Part, 0, count)
	for partNo := range count {
		rawPart, err := cursor.Raw()
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", partNo, err)
		}

		part, excludeNull, err := decodeIndexPart(rawPart, space)
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", partNo, err)
		}

		wildcardSuffix, hasWildcard, err := pathMultikeySuffix(part.path)
		if err != nil {
			return nil, fmt.Errorf("part %d path %q: %w", partNo, part.path, err)
		}

		part.wildcard = hasWildcard
		part.wildcardSuffix = wildcardSuffix

		parts = append(parts, part)
		index.ExcludeNull = index.ExcludeNull || excludeNull
		index.Multikey = index.Multikey || hasWildcard
	}

	return parts, nil
}

func decodeIndexPart(raw []byte, space *Space) (Part, bool, error) {
	legacy := format.NewMPCursor(raw)
	if count, err := legacy.ArrayLen(); err == nil {
		return decodeLegacyIndexPart(&legacy, count, space)
	}

	return decodeMapIndexPart(raw)
}

func decodeLegacyIndexPart(cursor *format.MPCursor, count int, space *Space) (Part, bool, error) {
	if count < legacyPartMinFields {
		return Part{}, false, fmt.Errorf("%w: legacy part needs field and type", ErrInvalidIndexRow)
	}

	fieldNo, err := cursorUint32(cursor, ErrInvalidIndexRow, "part field")
	if err != nil {
		return Part{}, false, err
	}

	typeName, err := cursor.Str()
	if err != nil {
		return Part{}, false, fmt.Errorf("part type: %w", err)
	}

	for field := legacyPartMinFields; field < count; field++ {
		if err := cursor.Skip(); err != nil {
			return Part{}, false, fmt.Errorf("part field %d: %w", field, err)
		}
	}

	part := Part{FieldNo: fieldNo, Type: string(typeName)}
	if int(fieldNo) < len(space.fieldNullable) {
		part.Nullable = space.fieldNullable[fieldNo]
	}

	return part, false, nil
}

func decodeMapIndexPart(raw []byte) (Part, bool, error) {
	cursor := format.NewMPCursor(raw)

	count, err := cursor.MapLen()
	if err != nil {
		return Part{}, false, fmt.Errorf("map: %w", err)
	}

	var part Part

	fieldSeen := false
	typeSeen := false
	excludeNull := false

	for entry := range count {
		key, err := cursor.Str()
		if err != nil {
			return Part{}, false, fmt.Errorf("key %d: %w", entry, err)
		}

		switch string(key) {
		case "field":
			part.FieldNo, err = cursorUint32(&cursor, ErrInvalidIndexRow, "part field")
			fieldSeen = err == nil
		case "type":
			var value []byte

			value, err = cursor.Str()
			if err == nil {
				part.Type = string(value)
				typeSeen = true
			}
		case "collation":
			part.Collation, err = decodeCollation(&cursor)
		case "is_nullable":
			part.Nullable, err = cursor.Bool()
		case "nullable_action":
			var value []byte

			value, err = cursor.Str()
			if err == nil {
				part.Nullable = string(value) == "none"
			}
		case "path":
			var value []byte

			value, err = cursor.Str()
			if err == nil {
				part.path = string(value)
			}
		case "exclude_null":
			excludeNull, err = cursor.Bool()
		default:
			err = cursor.Skip()
		}

		if err != nil {
			return Part{}, false, fmt.Errorf("option %q: %w", key, err)
		}
	}

	if !fieldSeen || !typeSeen {
		return Part{}, false, fmt.Errorf("%w: part map needs field and type", ErrInvalidIndexRow)
	}

	part.excludeNull = excludeNull

	return part, excludeNull, nil
}

func decodeCollation(cursor *format.MPCursor) (string, error) {
	raw, err := cursor.Raw()
	if err != nil {
		return "", fmt.Errorf("raw: %w", err)
	}

	value := format.NewMPCursor(raw)
	if id, err := value.Uint(); err == nil {
		return strconv.FormatUint(id, 10), nil
	}

	value = format.NewMPCursor(raw)

	name, err := value.Str()
	if err != nil {
		return "", fmt.Errorf("collation: %w", err)
	}

	return string(name), nil
}

func parseIndexType(name string) (IndexType, error) {
	switch strings.ToLower(name) {
	case "tree":
		return IndexTree, nil
	case "hash":
		return IndexHash, nil
	case "rtree":
		return IndexRTree, nil
	case "bitset":
		return IndexBitset, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownIndexType, name)
	}
}

func decodeFirstUint32(tuple []byte, sentinel error) (uint32, error) {
	cursor := format.NewMPCursor(tuple)

	count, err := cursor.ArrayLen()
	if err != nil {
		return 0, fmt.Errorf("%w: tuple: %w", sentinel, err)
	}

	if count < 1 {
		return 0, fmt.Errorf("%w: empty tuple", sentinel)
	}

	return cursorUint32(&cursor, sentinel, "id")
}

func decodeIndexKey(tuple []byte) (uint32, uint32, error) {
	cursor := format.NewMPCursor(tuple)

	count, err := cursor.ArrayLen()
	if err != nil {
		return 0, 0, fmt.Errorf("%w: tuple: %w", ErrInvalidIndexRow, err)
	}

	if count < indexKeyFieldCount {
		return 0, 0, fmt.Errorf("%w: got %d key fields, need %d", ErrInvalidIndexRow, count, indexKeyFieldCount)
	}

	spaceID, err := cursorUint32(&cursor, ErrInvalidIndexRow, "space id")
	if err != nil {
		return 0, 0, err
	}

	indexID, err := cursorUint32(&cursor, ErrInvalidIndexRow, "index id")
	if err != nil {
		return 0, 0, err
	}

	return spaceID, indexID, nil
}

func cursorUint32(cursor *format.MPCursor, sentinel error, field string) (uint32, error) {
	value, err := cursor.Uint()
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", sentinel, field, err)
	}

	if value > math.MaxUint32 {
		return 0, fmt.Errorf("%w: %s overflows uint32", sentinel, field)
	}

	return uint32(value), nil
}

func findIndex(indexes []Index, id uint32) (int, bool) {
	for pos := range indexes {
		if indexes[pos].ID == id {
			return pos, true
		}
	}

	return 0, false
}

func recomputeFieldMapSize(space *Space) {
	type slot struct {
		field uint32
		path  string
	}

	slots := make(map[slot]struct{})

	for indexNo := range space.Indexes {
		index := &space.Indexes[indexNo]
		if index.Functional || indexIsSequential(index) {
			continue
		}

		for partNo := range index.Parts {
			part := &index.Parts[partNo]
			if part.FieldNo > 0 || part.path != "" {
				slots[slot{field: part.FieldNo, path: part.path}] = struct{}{}
			}

			if pos := strings.Index(part.path, "[*]"); pos >= 0 {
				root := part.path[:pos+len("[*]")]
				slots[slot{field: part.FieldNo, path: root}] = struct{}{}
			}
		}
	}

	space.FieldMapSize = fieldMapSlotSize * len(slots)
}

func indexIsSequential(index *Index) bool {
	for partNo := range index.Parts {
		part := &index.Parts[partNo]
		if part.path != "" || part.FieldNo != uint32(partNo) {
			return false
		}
	}

	return true
}
