// Package dir indexes a directory of Tarantool journal files (xlog / snap /
// vylog / run / index). It scans by filename signature, validates each file's
// in-meta vclock against its filename, enforces Lamport-order
// consistency across the chain (mirrors xdir_index_file at
// src/box/xlog.c:382), and provides vclock / lsn lookup so a caller can
// locate the file that contains a given position.
//
// The filename rule is <vclock-signature>.<ext>, with signature equal to the
// arithmetic sum of per-replica LSNs (vclock_sum,
// src/lib/vclock/vclock.h:230).
//
// The package keeps no global state. Filesystem concerns
// (directory listing, file opening for meta) live here, not in `format`.
package dir
