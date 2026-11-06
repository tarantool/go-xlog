// Package format implements pure byte-level encoding and decoding of every
// on-disk artefact in Tarantool's journal envelope: meta header, fixheader,
// xrow records, tx blocks, the EOF marker, and per-body schemas
// (DML / RAFT / SYNCHRO / VYLOG / vinyl RUN and INDEX) and the operations
// of UPDATE and UPSERT. It is the foundation for the `reader`,
// `writer`, `dir`, `rotate`, and utility packages.
//
// Design references in the Tarantool 3.x tree:
//
//   - meta:        src/box/xlog.c:115-122,155
//   - fixheader:   src/box/iproto_constants.h:51, src/box/xlog.c:1095-1105
//   - magic bytes: src/box/xlog.c:73-75
//   - tx encoding: src/box/xlog.c:1080-1120 (uncompressed), :1128-1200 (zstd)
//   - xrow header: src/box/xrow.c:331-444
//   - tx semantics:src/box/xrow.c:259-263, 402-410
//   - CRC32C:      src/box/xlog.c:1086, src/lib/digest/crc32_impl.c:196-205
//   - vy_log keys: src/box/vy_log.c:71-89
//   - vinyl runs:  src/box/vy_run.c, src/box/vy_stmt.c
//   - vclock:      src/lib/vclock/vclock.h:54-80, src/lib/vclock/vclock.c:55-72
//   - update ops:  src/box/xrow_update_field.c (xrow_update_op_decode)
//
// No I/O. No global mutable state (sync.Pool instances are construction caches,
// not state).
//
// # Aliasing
//
// The decoders do not copy. Every []byte a decoder returns, and every []byte
// field of a struct it fills, is a sub-slice of its input buffer. This is the
// contract of the whole package, not a detail of one function, and it is
// what keeps the read path allocation-free: a caller that only needs the
// SpaceID of a row never pays for its tuple.
//
// The flip side is that a decoded value is only valid as long as its input
// is. Retaining an aliased slice past the next reuse of the buffer is a
// use-after-free in all but name: nothing fails, the bytes just change under
// it, and it reads as data corruption. A file reader hands the decoders a
// fresh region per block, so the window is wide and the bug hides; a wire
// consumer that reads every packet into one receive buffer sees the next row
// overwrite the tuple it kept from the last one.
//
// The slices that alias the input are, per type:
//
//   - XRow: BodyRaw (DecodeXRow, DecodeXRowInto). A body decoded from
//     BodyRaw aliases the same buffer in turn.
//   - DMLBody: Tuple, Key, Ops, OldTuple, NewTuple and Arrow.
//   - VyStmtBody: Tuple, Key and Ops.
//   - VyRunInfo: MinKey, MaxKey and Bloom.
//   - VyPageInfo: MinKey.
//   - VyLogBody: the values of Keys. The map itself is new on every decode.
//   - Operation: FieldName and Value (DecodeOps, DecodeOpsInto).
//   - DecodeOptions.OnUnknown: its raw argument, and so the values of the map
//     CollectUnknown fills.
//   - DecodeTxBlock: the row slices, which alias the plain payload, and the
//     plain payload itself for a block that is not compressed.
//
// Nothing else aliases: RaftBody and SynchroBody hold numbers and a VClock
// map built on every decode, DecodeVyRowIndexBody returns new offsets, and
// a VClockArray and a Fixheader are values.
//
// To keep a value past its buffer, copy it rather than its fields one by
// one. XRow, Operation and every body struct - RaftBody and SynchroBody
// too, for their maps - have Clone, which returns a deep copy sharing no
// memory with the input, and CloneInto, which deep-copies into a
// caller-owned value and reuses the capacity of its slices, so a loop that
// retains rows allocates nothing once warm. CloneOps and CloneOpsInto do
// the same for a slice of operations; a raw value of OnUnknown or a row
// slice is a plain []byte for bytes.Clone. A nil slice or map stays nil and
// an empty one stays empty, so a clone is reflect.DeepEqual to its source.
//
// CloneInto writes into the slices dst holds, so they must belong to dst:
// dst must be the zero value or hold what Clone or CloneInto last put in it.
// A dst that was decoded into aliases its buffer, and CloneInto would copy
// into that buffer; zero it first. A slice of dst that shares its array with
// the same field of the source - dst is the source, or a shallow copy of
// it - is replaced rather than reused, so CloneInto never leaves dst
// sharing memory with the source. A map is never reused, as the decoders
// never reuse one: the vclock of a RaftBody or a SynchroBody and the keys of
// a VyLogBody are new maps.
package format
