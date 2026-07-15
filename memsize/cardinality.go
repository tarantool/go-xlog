package memsize

import (
	"errors"
	"fmt"

	"github.com/tarantool/go-xlog/format"
)

// ErrInvalidIndexTuple marks malformed MessagePack encountered while
// evaluating an index key.
var ErrInvalidIndexTuple = errors.New("invalid indexed tuple")

// indexEntryCount mirrors tuple_raw_multikey_count() and
// tuple_key_is_excluded_slow() in src/box/tuple.c:627-647 and
// src/box/tuple_extract_key.cc:491-509.
func indexEntryCount(tuple []byte, index *Index) (uint32, error) {
	if index == nil {
		return 0, fmt.Errorf("memsize: index cardinality: %w: nil index", ErrInvalidIndexTuple)
	}

	if !index.Multikey {
		excluded, err := tupleKeyExcluded(tuple, index, -1)
		if err != nil {
			return 0, err
		}

		if excluded {
			return 0, nil
		}

		return 1, nil
	}

	root, found, err := multikeyRoot(tuple, index)
	if err != nil {
		return 0, err
	}

	if !found || isMPNil(root) {
		return 0, nil
	}

	count, array, err := mpArray(root)
	if err != nil {
		return 0, fmt.Errorf("memsize: index cardinality: multikey root: %w", err)
	}

	if !array {
		return 0, fmt.Errorf("memsize: index cardinality: multikey root: %w: expected array",
			ErrInvalidIndexTuple)
	}

	if !index.ExcludeNull {
		return uint32(count), nil //nolint:gosec // MessagePack array32 cardinality fits uint32.
	}

	return multikeyEntryCount(tuple, root, count, index)
}

func multikeyEntryCount(tuple, root []byte, count int, index *Index) (uint32, error) {
	for partNo := range index.Parts {
		part := &index.Parts[partNo]
		if !part.excludeNull {
			continue
		}

		_, wildcard, err := partMultikeySuffix(part)
		if err != nil {
			return 0, fmt.Errorf("memsize: index cardinality: part %d: %w", partNo, err)
		}

		if wildcard {
			continue
		}

		null, err := tuplePartIsNull(tuple, part)
		if err != nil {
			return 0, fmt.Errorf("memsize: index cardinality: part %d: %w", partNo, err)
		}

		if null {
			return 0, nil
		}
	}

	cursor := format.NewMPCursor(root)
	if _, err := cursor.ArrayLen(); err != nil {
		return 0, fmt.Errorf("memsize: index cardinality: multikey root: %w", err)
	}

	var entries uint32

	for multikeyIndex := range count {
		element, err := cursor.Raw()
		if err != nil {
			return 0, fmt.Errorf("memsize: index cardinality: multikey element %d: %w",
				multikeyIndex, err)
		}

		excluded := false

		for partNo := range index.Parts {
			part := &index.Parts[partNo]
			if !part.excludeNull {
				continue
			}

			suffix, wildcard, err := partMultikeySuffix(part)
			if err != nil {
				return 0, fmt.Errorf("memsize: index cardinality: part %d: %w", partNo, err)
			}

			if !wildcard {
				continue
			}

			value, found, _, err := resolveJSONPath(element, suffix, -1, false)
			if err != nil {
				return 0, fmt.Errorf("memsize: index cardinality: part %d suffix %q: %w",
					partNo, suffix, err)
			}

			if !found || isMPNil(value) {
				excluded = true

				break
			}
		}

		if !excluded {
			entries++
		}
	}

	return entries, nil
}

func multikeyRoot(tuple []byte, index *Index) ([]byte, bool, error) {
	for partNo := range index.Parts {
		part := &index.Parts[partNo]

		_, hasWildcard, err := partMultikeySuffix(part)
		if err != nil {
			return nil, false, fmt.Errorf("memsize: index cardinality: part %d: %w", partNo, err)
		}

		if !hasWildcard {
			continue
		}

		raw, found, err := tupleField(tuple, part.FieldNo)
		if err != nil {
			return nil, false, fmt.Errorf("memsize: index cardinality: part %d field %d: %w",
				partNo, part.FieldNo, err)
		}

		if !found || isMPNil(raw) {
			return nil, false, nil
		}

		raw, found, stopped, err := resolveJSONPath(raw, part.path, -1, true)
		if err != nil {
			return nil, false, fmt.Errorf("memsize: index cardinality: part %d path %q: %w",
				partNo, part.path, err)
		}

		if !found {
			return nil, false, nil
		}

		if !stopped {
			return nil, false, fmt.Errorf("memsize: index cardinality: part %d path %q: %w: missing [*]",
				partNo, part.path, ErrInvalidIndexPath)
		}

		return raw, found, nil
	}

	return nil, false, fmt.Errorf("memsize: index cardinality: %w: multikey index has no [*] part",
		ErrInvalidIndexPath)
}

func tupleKeyExcluded(tuple []byte, index *Index, multikeyIndex int) (bool, error) {
	if !index.ExcludeNull {
		return false, nil
	}

	for partNo := range index.Parts {
		part := &index.Parts[partNo]
		if !part.excludeNull {
			continue
		}

		null, err := tuplePartIsNullAt(tuple, part, multikeyIndex)
		if err != nil {
			return false, fmt.Errorf("memsize: index cardinality: part %d: %w", partNo, err)
		}

		if null {
			return true, nil
		}
	}

	return false, nil
}

func tuplePartIsNull(tuple []byte, part *Part) (bool, error) {
	return tuplePartIsNullAt(tuple, part, -1)
}

func tuplePartIsNullAt(tuple []byte, part *Part, multikeyIndex int) (bool, error) {
	raw, found, err := tupleField(tuple, part.FieldNo)
	if err != nil {
		return false, fmt.Errorf("field %d: %w", part.FieldNo, err)
	}

	if found && part.path != "" {
		raw, found, _, err = resolveJSONPath(raw, part.path, multikeyIndex, false)
		if err != nil {
			return false, fmt.Errorf("path %q: %w", part.path, err)
		}
	}

	return !found || isMPNil(raw), nil
}

func tupleField(tuple []byte, fieldNo uint32) ([]byte, bool, error) {
	cursor := format.NewMPCursor(tuple)

	count, err := cursor.ArrayLen()
	if err != nil {
		return nil, false, fmt.Errorf("%w: tuple array: %w", ErrInvalidIndexTuple, err)
	}

	if uint64(fieldNo) >= uint64(count) { //nolint:gosec // MessagePack array counts are non-negative.
		return nil, false, nil
	}

	for current := range fieldNo {
		if err := cursor.Skip(); err != nil {
			return nil, false, fmt.Errorf("%w: tuple field %d: %w", ErrInvalidIndexTuple, current, err)
		}
	}

	raw, err := cursor.Raw()
	if err != nil {
		return nil, false, fmt.Errorf("%w: tuple field %d: %w", ErrInvalidIndexTuple, fieldNo, err)
	}

	return raw, true, nil
}

func resolveJSONPath(
	raw []byte,
	path string,
	multikeyIndex int,
	stopAtWildcard bool,
) ([]byte, bool, bool, error) {
	lexer := jsonPathLexer{path: path}

	for {
		token, err := lexer.next()
		if err != nil {
			return nil, false, false, err
		}

		switch token.kind {
		case jsonPathEnd:
			return raw, true, false, nil
		case jsonPathAny:
			if stopAtWildcard {
				return raw, true, true, nil
			}

			if multikeyIndex < 0 {
				return nil, false, false, fmt.Errorf("%w: [*] needs a multikey offset", ErrInvalidIndexPath)
			}

			raw, err = mpArrayValue(raw, multikeyIndex)
		case jsonPathIndex:
			raw, err = mpArrayValue(raw, token.index)
		case jsonPathKey:
			raw, _, err = mpMapValue(raw, token.key)
		}

		if err != nil {
			return nil, false, false, err
		}

		if raw == nil {
			return nil, false, false, nil
		}
	}
}

func mpArrayValue(raw []byte, index int) ([]byte, error) {
	if isMPNil(raw) {
		return nil, nil
	}

	count, array, err := mpArray(raw)
	if err != nil {
		return nil, err
	}

	if !array || index >= count {
		return nil, nil
	}

	cursor := format.NewMPCursor(raw)
	if _, err := cursor.ArrayLen(); err != nil {
		return nil, fmt.Errorf("%w: array: %w", ErrInvalidIndexTuple, err)
	}

	for current := range index {
		if err := cursor.Skip(); err != nil {
			return nil, fmt.Errorf("%w: array element %d: %w", ErrInvalidIndexTuple, current, err)
		}
	}

	value, err := cursor.Raw()
	if err != nil {
		return nil, fmt.Errorf("%w: array element %d: %w", ErrInvalidIndexTuple, index, err)
	}

	return value, nil
}

func mpMapValue(raw []byte, key string) ([]byte, bool, error) {
	if isMPNil(raw) || !isMPMap(raw) {
		return nil, false, nil
	}

	cursor := format.NewMPCursor(raw)

	count, err := cursor.MapLen()
	if err != nil {
		return nil, false, fmt.Errorf("%w: map: %w", ErrInvalidIndexTuple, err)
	}

	for entry := range count {
		rawKey, err := cursor.Raw()
		if err != nil {
			return nil, false, fmt.Errorf("%w: map key %d: %w", ErrInvalidIndexTuple, entry, err)
		}

		if isMPString(rawKey) {
			keyCursor := format.NewMPCursor(rawKey)

			decoded, err := keyCursor.Str()
			if err != nil {
				return nil, false, fmt.Errorf("%w: map key %d: %w", ErrInvalidIndexTuple, entry, err)
			}

			if string(decoded) == key {
				value, err := cursor.Raw()
				if err != nil {
					return nil, false, fmt.Errorf("%w: map value %d: %w", ErrInvalidIndexTuple, entry, err)
				}

				return value, true, nil
			}
		}

		if err := cursor.Skip(); err != nil {
			return nil, false, fmt.Errorf("%w: map value %d: %w", ErrInvalidIndexTuple, entry, err)
		}
	}

	return nil, false, nil
}

func mpArray(raw []byte) (int, bool, error) {
	if !isMPArray(raw) {
		return 0, false, nil
	}

	cursor := format.NewMPCursor(raw)

	count, err := cursor.ArrayLen()
	if err != nil {
		return 0, true, fmt.Errorf("%w: array: %w", ErrInvalidIndexTuple, err)
	}

	return count, true, nil
}

func isMPNil(raw []byte) bool {
	return len(raw) > 0 && raw[0] == mpNil
}

func isMPArray(raw []byte) bool {
	return len(raw) > 0 &&
		(raw[0] >= mpFixArrayBase && raw[0] < mpFixStrBase || raw[0] == mpArray16 || raw[0] == mpArray32)
}

func isMPMap(raw []byte) bool {
	return len(raw) > 0 &&
		(raw[0] >= mpFixMapBase && raw[0] < mpFixArrayBase || raw[0] == mpMap16 || raw[0] == mpMap32)
}

func isMPString(raw []byte) bool {
	return len(raw) > 0 &&
		(raw[0] >= mpFixStrBase && raw[0] <= mpFixStrBase+mpFixStrMax ||
			raw[0] == mpStr8 || raw[0] == mpStr16 || raw[0] == mpStr32)
}

func partMultikeySuffix(part *Part) (string, bool, error) {
	if part.wildcard {
		return part.wildcardSuffix, true, nil
	}

	return pathMultikeySuffix(part.path)
}
