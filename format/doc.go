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
package format
