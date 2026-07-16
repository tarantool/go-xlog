package format

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tarantool/go-iproto"
)

// Filetype is the leading text token in the meta header (`XLOG`, `SNAP`,
// `VYLOG`, `RUN`, `INDEX`). It is the discriminator the reader uses to pick
// which body decoders are valid for the rows inside.
type Filetype string

// Filetype constants — match the literal strings Tarantool writes in
// src/box/xlog.c:155.
const (
	FiletypeXLOG  Filetype = "XLOG"
	FiletypeSNAP  Filetype = "SNAP"
	FiletypeVYLOG Filetype = "VYLOG"
	FiletypeRUN   Filetype = "RUN"
	FiletypeINDEX Filetype = "INDEX"
)

// ErrUnknownFiletype is returned by Filetype.Ext for a Filetype that
// does not correspond to a Tarantool on-disk artefact.
var ErrUnknownFiletype = errors.New("format: unknown filetype")

// Ext returns the on-disk extension Tarantool uses for this Filetype.
// Mirrors xdir_open (src/box/xlog.c:267-274).
func (ft Filetype) Ext() (string, error) {
	switch ft {
	case FiletypeXLOG:
		return ".xlog", nil
	case FiletypeSNAP:
		return ".snap", nil
	case FiletypeVYLOG:
		return ".vylog", nil
	case FiletypeRUN:
		return ".run", nil
	case FiletypeINDEX:
		return ".index", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownFiletype, string(ft))
	}
}

// Magic byte sequences — the first 4 bytes of every tx block and the EOF
// marker. Stored big-endian on disk; we treat them as opaque [4]byte to
// avoid host-endianness ambiguity. See src/box/xlog.c:73-75.
var (
	// RowMarker prefixes an uncompressed tx block.
	RowMarker = [4]byte{0xD5, 0xBA, 0x0B, 0xAB}
	// ZRowMarker prefixes a zstd-compressed tx block.
	ZRowMarker = [4]byte{0xD5, 0xBA, 0x0B, 0xBA}
	// EOFMarker terminates a complete xlog/snap/vylog file.
	EOFMarker = [4]byte{0xD5, 0x10, 0xAD, 0xED}
)

// Layout constants. See src/box/iproto_constants.h:51 and src/box/xlog.c:85,93.
const (
	// MarkerSize is the byte length of every magic marker (RowMarker,
	// ZRowMarker, EOFMarker) — the fixed prefix a reader peeks to classify
	// what comes next.
	MarkerSize = 4

	// FixheaderSize is the exact on-disk size of every tx-block fixheader.
	// XLOG_FIXHEADER_SIZE in Tarantool. The fixheader is zero-padded as an
	// mp_str so the total always reaches this value.
	FixheaderSize = 19

	// CompressThreshold is the minimum payload size (XLOG_TX_COMPRESS_THRESHOLD)
	// above which a tx is zstd-compressed.
	CompressThreshold = 2 * 1024

	// AutocommitThreshold is the in-memory buffer size after which the writer
	// auto-flushes the current tx on the next row boundary
	// (XLOG_TX_AUTOCOMMIT_THRESHOLD, src/box/xlog.c:85).
	AutocommitThreshold = 128 * 1024

	// ZstdLevel is the compression level Tarantool uses
	// (ZSTD_compressBegin(ctx, 3), src/box/xlog.c:1135).
	ZstdLevel = 3

	// FormatVersion is the literal "0.13" string written in the meta header
	// for current files. The writer always emits this.
	FormatVersion = "0.13"

	// LegacyFormatVersion is the older "0.12" string the reader accepts when
	// AcceptV012 is set on MetaOptions.
	LegacyFormatVersion = "0.12"
)

// The IPROTO header keys, DML body keys, request types, and header flags this
// envelope uses are taken directly from github.com/tarantool/go-iproto — the
// authoritative, generated source for Tarantool's protocol numbers. Use the
// iproto.Key / iproto.Type / iproto.Flag / iproto.RaftKey constants
// (iproto.IPROTO_*) throughout; the format package no longer maintains its own
// parallel copies. The only protocol numbers kept here are the vy_log body
// keys below, which go-iproto does not cover.

// TypeName returns the short Tarantool name of a durable request type
// ("INSERT", "REPLACE", "RAFT", …). It is iproto.Type.String() with the
// "IPROTO_" prefix trimmed; unknown types render as iproto's Type(n) form.
func TypeName(t iproto.Type) string {
	return strings.TrimPrefix(t.String(), "IPROTO_")
}

// VyKey is a vy_log body-map key. The VYLOG body is a msgpack map keyed by
// these integers (Tarantool's vy_log_key enum). Go-iproto does not cover
// these, so they are defined here. See src/box/vy_log.c:71-89.
type VyKey uint64

// vy_log body keys (Tarantool's vy_log_key enum, src/box/vy_log.c:71-89).
const (
	VyKeyLSMID         VyKey = 0
	VyKeyRangeID       VyKey = 1
	VyKeyRunID         VyKey = 2
	VyKeyBegin         VyKey = 3
	VyKeyEnd           VyKey = 4
	VyKeyIndexID       VyKey = 5
	VyKeySpaceID       VyKey = 6
	VyKeyDef           VyKey = 7
	VyKeySliceID       VyKey = 8
	VyKeyDumpLSN       VyKey = 9
	VyKeyGCLSN         VyKey = 10
	VyKeyTruncateCount VyKey = 11
	VyKeyCreateLSN     VyKey = 12
	VyKeyModifyLSN     VyKey = 13
	VyKeyDropLSN       VyKey = 14
	VyKeyGroupID       VyKey = 15
	VyKeyDumpCount     VyKey = 16
)

// vyKeyNames maps recognised vy_log keys to their short names.
var vyKeyNames = map[VyKey]string{
	VyKeyLSMID:         "LSM_ID",
	VyKeyRangeID:       "RANGE_ID",
	VyKeyRunID:         "RUN_ID",
	VyKeyBegin:         "BEGIN",
	VyKeyEnd:           "END",
	VyKeyIndexID:       "INDEX_ID",
	VyKeySpaceID:       "SPACE_ID",
	VyKeyDef:           "DEF",
	VyKeySliceID:       "SLICE_ID",
	VyKeyDumpLSN:       "DUMP_LSN",
	VyKeyGCLSN:         "GC_LSN",
	VyKeyTruncateCount: "TRUNCATE_COUNT",
	VyKeyCreateLSN:     "CREATE_LSN",
	VyKeyModifyLSN:     "MODIFY_LSN",
	VyKeyDropLSN:       "DROP_LSN",
	VyKeyGroupID:       "GROUP_ID",
	VyKeyDumpCount:     "DUMP_COUNT",
}

// String returns the short vy_log key name (e.g. "LSM_ID"), or VyKey(n) for
// an unrecognised key.
func (k VyKey) String() string {
	if name, ok := vyKeyNames[k]; ok {
		return name
	}

	return fmt.Sprintf("VyKey(%d)", uint64(k))
}
