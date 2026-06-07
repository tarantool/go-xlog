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

import (
	"github.com/tarantool/go-xlog/format"
)

// FileEntry describes one indexed file inside a Dir. The fields are filled
// from the file's name (Signature, Path, Filetype-via-extension) and from
// its parsed meta header (VClock, PrevVClock).
//
// FileEntry is a value type and safe to copy. The VClock / PrevVClock maps
// are not deep-copied on Files() return — callers must not mutate them.
type FileEntry struct {
	// Path is the absolute or relative path that OpenDir was called with,
	// joined with the file's basename. Suitable for passing directly to
	// reader.Open.
	Path string

	// Signature is the integer parsed from the filename's <digits>.<ext>
	// stem. It equals VClock.Signature().
	Signature int64

	// VClock is the file's in-meta VClock (the high-water vector at the
	// time the file was opened).
	VClock format.VClock

	// PrevVClock is the file's in-meta PrevVClock (the chain link to the
	// previous file's VClock — empty for SNAP and for the very first file
	// in a chain).
	PrevVClock format.VClock

	// Filetype mirrors the OpenDir filter parameter; redundant with the
	// file extension but convenient for callers that pass FileEntry along
	// further into the stack.
	Filetype format.Filetype
}
