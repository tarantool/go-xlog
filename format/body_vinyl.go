package format

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/tarantool/go-iproto"
)

// The rows of the vinyl run (.run) and index (.index) files. Their row types
// are iproto.VY_INDEX_RUN_INFO, iproto.VY_INDEX_PAGE_INFO and
// iproto.VY_RUN_ROW_INDEX, which Tarantool reserves next to the IPROTO request
// types (src/box/iproto_constants.h).
//
// A .run file holds the statements of one run of one LSM tree (one index of a
// vinyl space), sorted by key, in pages. A page is one tx block, zstd-
// compressed when it outgrows CompressThreshold: its statements - INSERT,
// REPLACE, DELETE and UPSERT rows decoded by DecodeVyStmtBody - followed by a
// VY_RUN_ROW_INDEX row decoded by DecodeVyRowIndexBody. A page is read whole
// with NextBlockRaw of a reader and DecodeTxBlock; the row cursors of a
// reader yield its rows one by one.
//
// A .index file is one tx block: a VY_INDEX_RUN_INFO row decoded by
// DecodeVyRunInfoBody, then a VY_INDEX_PAGE_INFO row decoded by
// DecodeVyPageInfoBody per page of the run.
//
// The header of a statement carries only its type and its LSN, which is the
// vclock signature (the sum of the vclock components) of the transaction
// that wrote it, not the LSN of the replica: every statement of a
// transaction has the same LSN, and the replica id is 0. A row without
// IPROTO_TSN decodes as a transaction of its own, so a statement has
// TSN == LSN and IsCommit, which says nothing about the transaction it came
// from. The statements are in key order, and for a key from the newest to the
// oldest; only statements a read view or an older run may still need survive
// a dump or a compaction.

// Keys of the body of a VY_INDEX_RUN_INFO row (vy_run_info_key,
// src/box/iproto_constants.h).
const (
	vyRunInfoMinKey             = 1
	vyRunInfoMaxKey             = 2
	vyRunInfoMinLSN             = 3
	vyRunInfoMaxLSN             = 4
	vyRunInfoPageCount          = 5
	vyRunInfoBloomFilterLegacy1 = 6
	vyRunInfoBloomFilterLegacy2 = 7
	vyRunInfoStmtStat           = 8
	vyRunInfoBloomFilter        = 9
)

// Keys of the body of a VY_INDEX_PAGE_INFO row (vy_page_info_key,
// src/box/iproto_constants.h).
const (
	vyPageInfoOffset         = 1
	vyPageInfoSize           = 2
	vyPageInfoUnpackedSize   = 3
	vyPageInfoRowCount       = 4
	vyPageInfoMinKey         = 5
	vyPageInfoRowIndexOffset = 6
)

// vyRowIndexData is the key of the body of a VY_RUN_ROW_INDEX row
// (VY_ROW_INDEX_DATA).
const vyRowIndexData = 1

// vyStmtMetaFlags is the key of the statement flags in the IPROTO_TUPLE_META
// map of a statement (VY_STMT_FLAGS, src/box/vy_stmt.c).
const vyStmtMetaFlags = 1

// Sentinel errors of the vinyl body decoders.
var (
	// ErrVyMissingKey is returned for a run info or a page info row without
	// a key Tarantool requires.
	ErrVyMissingKey = errors.New("missing mandatory key")
	// ErrVyRowIndex is returned for a row index whose data is not a whole
	// number of offsets.
	ErrVyRowIndex = errors.New("bad row index")
)

// VyStmtFlags are the persistent flags of a vinyl statement (vy_stmt_flags,
// src/box/vy_stmt.h).
type VyStmtFlags uint64

const (
	// VyStmtDeferredDelete marks a REPLACE or DELETE of a primary index that
	// skipped deleting the old tuple from the secondary indexes: compaction
	// of the primary index generates those DELETEs. Only primary index runs
	// carry it.
	VyStmtDeferredDelete VyStmtFlags = 1 << 0
	// VyStmtSkipRead marks a statement reads ignore: the deferred DELETEs
	// compaction of the primary index writes to a secondary index.
	VyStmtSkipRead VyStmtFlags = 1 << 1
)

// VyStmtBody is the typed body of a statement of a .run file: an INSERT,
// REPLACE, DELETE or UPSERT row written by Tarantool's vy_stmt_encode_primary
// or vy_stmt_encode_secondary (src/box/vy_stmt.c). It is a DML body without a
// space id, plus the statement flags.
type VyStmtBody struct {
	// SpaceID is the IPROTO_SPACE_ID of the row. Tarantool does not write it
	// to a run, so it is normally 0; the space and index of a run are in its
	// path, <vinyl_dir>/<space_id>/<index_id>/<run_id>.run.
	SpaceID uint32
	// Tuple is the IPROTO_TUPLE of an INSERT, REPLACE or UPSERT, raw msgpack.
	// In a primary index run it is the whole tuple; in a secondary index run
	// it is the key of the tuple: the parts of the secondary index followed
	// by those of the primary one.
	Tuple []byte
	// Key is the IPROTO_KEY of a DELETE, raw msgpack: the key the DELETE
	// removes, in the parts of the index (as Tuple of a secondary index).
	Key []byte
	// Ops is the IPROTO_OPS of an UPSERT, raw msgpack. Unlike the operations
	// of an UPSERT request it is an array of operation lists, one per UPSERT
	// squashed into the statement, oldest first: when the key does not exist,
	// Tuple is inserted and the first list is not applied, and each other list
	// is applied in turn. Field numbers are 0-based.
	Ops []byte
	// Flags are the statement flags of IPROTO_TUPLE_META.
	Flags VyStmtFlags
	// Extras holds every unrecognised body key as raw msgpack.
	Extras map[uint64][]byte
}

// DecodeVyStmtBody parses the body of a statement of a .run file. It is
// DecodeVyStmtBodyInto into a new VyStmtBody.
func DecodeVyStmtBody(b []byte) (*VyStmtBody, error) {
	body := &VyStmtBody{}

	if err := DecodeVyStmtBodyInto(b, body); err != nil {
		return nil, err
	}

	return body, nil
}

// DecodeVyStmtBodyInto parses the body of a statement of a .run file into
// dst, the zero-allocation core of DecodeVyStmtBody. It first resets dst to
// the zero VyStmtBody, so a dst reused across statements carries no field of
// a prior one, then sets the fields of the keys b holds. The raw fields of
// dst (Tuple, Key, Ops, the values of Extras) alias b; clone one before
// retaining it past b's lifetime. A map of a prior statement is never
// reused: a body with a key that has no field allocates a new Extras map. On
// error dst holds what was decoded before it.
func DecodeVyStmtBodyInto(b []byte, dst *VyStmtBody) error {
	*dst = VyStmtBody{}
	body := dst

	return walkBody("DecodeVyStmtBody", b, func(key uint64, val []byte) error {
		switch iproto.Key(key) { //nolint:exhaustive,gosec // G115: msgpack body key is a small protocol number
		case iproto.IPROTO_SPACE_ID:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("space_id: %w", err)
			}

			body.SpaceID = uint32(v) //nolint:gosec // G115: space_id is a uint32 field per iproto_constants.h
		case iproto.IPROTO_TUPLE:
			body.Tuple = val
		case iproto.IPROTO_KEY:
			body.Key = val
		case iproto.IPROTO_OPS:
			body.Ops = val
		case iproto.IPROTO_TUPLE_META:
			flags, err := decodeVyStmtMeta(val)
			if err != nil {
				return fmt.Errorf("tuple_meta: %w", err)
			}

			body.Flags = flags
		default:
			if body.Extras == nil {
				body.Extras = map[uint64][]byte{}
			}

			body.Extras[key] = val
		}

		return nil
	})
}

// decodeVyStmtMeta reads the flags of the IPROTO_TUPLE_META map of a
// statement; vy_stmt_meta_decode ignores any other key, and so does this.
func decodeVyStmtMeta(b []byte) (VyStmtFlags, error) {
	var flags VyStmtFlags

	err := walkMap(b, func(key uint64, val []byte) error {
		if key != vyStmtMetaFlags {
			return nil
		}

		v, _, err := readMPUint(val)
		if err != nil {
			return fmt.Errorf("flags: %w", err)
		}

		flags = VyStmtFlags(v)

		return nil
	})

	return flags, err
}

// VyStmtStat counts the statements of a run by type.
type VyStmtStat struct {
	Inserts  uint64
	Replaces uint64
	Deletes  uint64
	Upserts  uint64
}

// VyRunInfo is the typed body of a VY_INDEX_RUN_INFO row, the first row of a
// .index file (vy_run_info_encode, src/box/vy_run.c).
type VyRunInfo struct {
	// MinKey and MaxKey are the smallest and the greatest key of the run,
	// raw msgpack arrays.
	MinKey []byte
	MaxKey []byte
	// MinLSN and MaxLSN bound the LSNs of the statements of the run.
	MinLSN int64
	MaxLSN int64
	// PageCount is the number of pages of the run, and of VY_INDEX_PAGE_INFO
	// rows after this one.
	PageCount uint32
	// BloomVersion is the version of the bloom filter: 3 for the current
	// one, 1 or 2 for the legacy ones, 0 when the run has none.
	BloomVersion uint8
	// Bloom is the bloom filter of the keys of the run, raw msgpack.
	Bloom []byte
	// StmtStat counts the statements of the run by type.
	StmtStat VyStmtStat
	// Extras holds every unrecognised key as raw msgpack.
	Extras map[uint64][]byte
}

// DecodeVyRunInfoBody parses the body of a VY_INDEX_RUN_INFO row. Like
// Tarantool's vy_run_info_decode it requires the keys, the LSNs and the page
// count. It is DecodeVyRunInfoBodyInto into a new VyRunInfo.
func DecodeVyRunInfoBody(b []byte) (*VyRunInfo, error) {
	info := &VyRunInfo{}

	if err := DecodeVyRunInfoBodyInto(b, info); err != nil {
		return nil, err
	}

	return info, nil
}

// vyRunInfoRequired are the keys vy_run_info_decode requires, in key order.
var vyRunInfoRequired = []vyRequiredKey{
	{vyRunInfoMinKey, "min_key"}, {vyRunInfoMaxKey, "max_key"},
	{vyRunInfoMinLSN, "min_lsn"}, {vyRunInfoMaxLSN, "max_lsn"},
	{vyRunInfoPageCount, "page_count"},
}

// DecodeVyRunInfoBodyInto parses the body of a VY_INDEX_RUN_INFO row into
// dst, the zero-allocation core of DecodeVyRunInfoBody. It first resets dst to
// the zero VyRunInfo, so a reused dst carries no field of a prior row, then
// sets the fields of the keys b holds. The raw fields of dst (MinKey, MaxKey,
// Bloom, the values of Extras) alias b; clone one before retaining it past
// b's lifetime. A map of a prior row is never reused: a body with a key that
// has no field allocates a new Extras map. On error dst holds what was
// decoded before it.
func DecodeVyRunInfoBodyInto(b []byte, dst *VyRunInfo) error {
	const name = "DecodeVyRunInfoBody"

	*dst = VyRunInfo{}
	info := dst

	var seen vyKeySet

	err := walkBody(name, b, func(key uint64, val []byte) error {
		seen.add(key)

		switch key {
		case vyRunInfoMinKey:
			info.MinKey = val
		case vyRunInfoMaxKey:
			info.MaxKey = val
		case vyRunInfoMinLSN, vyRunInfoMaxLSN, vyRunInfoPageCount:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("key %d: %w", key, err)
			}

			switch key {
			case vyRunInfoMinLSN:
				info.MinLSN = int64(v) //nolint:gosec // G115: an LSN fits int64 in the xlog format
			case vyRunInfoMaxLSN:
				info.MaxLSN = int64(v) //nolint:gosec // G115: an LSN fits int64 in the xlog format
			default:
				info.PageCount = uint32(v) //nolint:gosec // G115: page_count is a uint32 field of vy_run_info
			}
		case vyRunInfoBloomFilterLegacy1, vyRunInfoBloomFilterLegacy2, vyRunInfoBloomFilter:
			info.Bloom = val
			info.BloomVersion = bloomVersion(key)
		case vyRunInfoStmtStat:
			stat, err := decodeVyStmtStat(val)
			if err != nil {
				return fmt.Errorf("stmt_stat: %w", err)
			}

			info.StmtStat = stat
		default:
			if info.Extras == nil {
				info.Extras = map[uint64][]byte{}
			}

			info.Extras[key] = val
		}

		return nil
	})
	if err != nil {
		return err
	}

	return missingKey(name, vyRunInfoRequired, seen)
}

// bloomVersion is the version of the bloom filter under a run info key
// (iproto_to_tuple_bloom_version, src/box/vy_run.c).
func bloomVersion(key uint64) uint8 {
	switch key {
	case vyRunInfoBloomFilterLegacy1:
		return 1
	case vyRunInfoBloomFilterLegacy2:
		return 2 //nolint:mnd // The second legacy version.
	default:
		return 3 //nolint:mnd // The current version.
	}
}

// decodeVyStmtStat reads the {type: count} map of the statement statistics
// of a run; vy_stmt_stat_decode ignores any other type, and so does this.
func decodeVyStmtStat(b []byte) (VyStmtStat, error) {
	var stat VyStmtStat

	err := walkMap(b, func(key uint64, val []byte) error {
		v, _, err := readMPUint(val)
		if err != nil {
			return fmt.Errorf("type %d: %w", key, err)
		}

		switch iproto.Type(key) { //nolint:exhaustive,gosec // G115: a request type is a small protocol number
		case iproto.IPROTO_INSERT:
			stat.Inserts = v
		case iproto.IPROTO_REPLACE:
			stat.Replaces = v
		case iproto.IPROTO_DELETE:
			stat.Deletes = v
		case iproto.IPROTO_UPSERT:
			stat.Upserts = v
		}

		return nil
	})

	return stat, err
}

// VyPageInfo is the typed body of a VY_INDEX_PAGE_INFO row of a .index file
// (vy_page_info_encode, src/box/vy_run.c).
type VyPageInfo struct {
	// Offset is the offset of the page in the .run file: where its tx block
	// starts.
	Offset uint64
	// Size is the size of the tx block of the page in the file.
	Size uint32
	// UnpackedSize is the size of the rows of the page, uncompressed.
	UnpackedSize uint32
	// RowCount is the number of statements of the page.
	RowCount uint32
	// MinKey is the smallest key of the page, a raw msgpack array.
	MinKey []byte
	// RowIndexOffset is the offset of the VY_RUN_ROW_INDEX row in the rows of
	// the page, uncompressed.
	RowIndexOffset uint32
	// Extras holds every unrecognised key as raw msgpack.
	Extras map[uint64][]byte
}

// DecodeVyPageInfoBody parses the body of a VY_INDEX_PAGE_INFO row. Like
// Tarantool's vy_page_info_decode it requires every key. It is
// DecodeVyPageInfoBodyInto into a new VyPageInfo.
func DecodeVyPageInfoBody(b []byte) (*VyPageInfo, error) {
	page := &VyPageInfo{}

	if err := DecodeVyPageInfoBodyInto(b, page); err != nil {
		return nil, err
	}

	return page, nil
}

// vyPageInfoRequired are the keys vy_page_info_decode requires, in key order.
var vyPageInfoRequired = []vyRequiredKey{
	{vyPageInfoOffset, "offset"}, {vyPageInfoSize, "size"},
	{vyPageInfoUnpackedSize, "unpacked_size"}, {vyPageInfoRowCount, "row_count"},
	{vyPageInfoMinKey, "min_key"}, {vyPageInfoRowIndexOffset, "row_index_offset"},
}

// DecodeVyPageInfoBodyInto parses the body of a VY_INDEX_PAGE_INFO row into
// dst, the zero-allocation core of DecodeVyPageInfoBody. It first resets dst
// to the zero VyPageInfo, so a reused dst carries no field of a prior row,
// then sets the fields of the keys b holds. The raw fields of dst (MinKey,
// the values of Extras) alias b; clone one before retaining it past b's
// lifetime. A map of a prior row is never reused: a body with a key that has
// no field allocates a new Extras map. On error dst holds what was decoded
// before it.
func DecodeVyPageInfoBodyInto(b []byte, dst *VyPageInfo) error {
	const name = "DecodeVyPageInfoBody"

	*dst = VyPageInfo{}
	page := dst

	var seen vyKeySet

	err := walkBody(name, b, func(key uint64, val []byte) error {
		seen.add(key)

		switch key {
		case vyPageInfoMinKey:
			page.MinKey = val
		case vyPageInfoOffset, vyPageInfoSize, vyPageInfoUnpackedSize,
			vyPageInfoRowCount, vyPageInfoRowIndexOffset:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("key %d: %w", key, err)
			}

			switch key {
			case vyPageInfoOffset:
				page.Offset = v
			case vyPageInfoSize:
				page.Size = uint32(v) //nolint:gosec // G115: a uint32 field of vy_page_info
			case vyPageInfoUnpackedSize:
				page.UnpackedSize = uint32(v) //nolint:gosec // G115: a uint32 field of vy_page_info
			case vyPageInfoRowCount:
				page.RowCount = uint32(v) //nolint:gosec // G115: a uint32 field of vy_page_info
			default:
				page.RowIndexOffset = uint32(v) //nolint:gosec // G115: a uint32 field of vy_page_info
			}
		default:
			if page.Extras == nil {
				page.Extras = map[uint64][]byte{}
			}

			page.Extras[key] = val
		}

		return nil
	})
	if err != nil {
		return err
	}

	return missingKey(name, vyPageInfoRequired, seen)
}

// DecodeVyRowIndexBody parses the body of a VY_RUN_ROW_INDEX row: the offset of
// each statement of the page in the rows of the page, uncompressed
// (vy_row_index_encode, src/box/vy_run.c).
func DecodeVyRowIndexBody(b []byte) ([]uint32, error) {
	const name = "DecodeVyRowIndexBody"

	var offsets []uint32

	err := walkBody(name, b, func(key uint64, val []byte) error {
		if key != vyRowIndexData {
			return nil
		}

		data, err := readMPBin(val)
		if err != nil {
			return fmt.Errorf("data: %w", err)
		}

		const width = 4 // Every offset is a big-endian uint32.
		if len(data)%width != 0 {
			return fmt.Errorf("%w: %d bytes", ErrVyRowIndex, len(data))
		}

		offsets = make([]uint32, len(data)/width)
		for i := range offsets {
			offsets[i] = binary.BigEndian.Uint32(data[i*width:])
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return offsets, nil
}

// walkBody checks that the body b of decoder name is not empty, walks its map
// with walkMap and prefixes an error with the decoder name.
func walkBody(name string, b []byte, fn func(key uint64, val []byte) error) error {
	if len(b) == 0 {
		return fmt.Errorf("format: %s: %w", name, ErrEmptyBody)
	}

	err := walkMap(b, fn)
	if err != nil {
		return fmt.Errorf("format: %s: %w", name, err)
	}

	return nil
}

// walkMap calls fn with each uint key of the msgpack map at b and the raw
// msgpack of its value.
func walkMap(b []byte, fn func(key uint64, val []byte) error) error {
	entries, off, err := readMPMapLen(b)
	if err != nil {
		return err
	}

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return fmt.Errorf("key %d: %w", i, err)
		}

		off += n

		valLen, err := skipMP(b[off:])
		if err != nil {
			return fmt.Errorf("skip value for key %d: %w", key, err)
		}

		err = fn(key, b[off:off+valLen])
		if err != nil {
			return err
		}

		off += valLen
	}

	return nil
}

// vyRequiredKey is a key a vinyl body requires and its name in an error.
type vyRequiredKey struct {
	key  uint64
	name string
}

// vyKeySet is a set of the keys below 64 a vinyl body holds, one bit per key:
// it tracks the required keys with no allocation.
type vyKeySet uint64

// add adds key to the set; a key of 64 or more is not one a body requires.
func (s *vyKeySet) add(key uint64) {
	if key < 64 { //nolint:mnd // The bits of a vyKeySet.
		*s |= 1 << key
	}
}

// missingKey reports the smallest key of required, the mandatory keys of a
// body in key order, that seen lacks, as Tarantool does.
func missingKey(name string, required []vyRequiredKey, seen vyKeySet) error {
	for _, r := range required {
		if seen&(1<<r.key) == 0 {
			return fmt.Errorf("format: %s: %w %s", name, ErrVyMissingKey, r.name)
		}
	}

	return nil
}
