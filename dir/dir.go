package dir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// Sentinel errors. Callers are expected to use errors.Is for matching.
var (
	// ErrNotFound is returned by LocateVClock / LocateLSN when no entry in
	// the directory contains the requested position (e.g. the target sits
	// strictly before the earliest indexed file, or the directory is empty).
	ErrNotFound = errors.New("dir: no file contains target")

	// ErrSignatureMismatch is returned by OpenDir when a file's filename
	// signature (the <digits>.<ext> stem parsed as int64) does not equal
	// the in-meta VClock.Signature() for that file. Tarantool's
	// xdir_open_cursor (src/box/xlog.c:451) treats this as fatal; so do we.
	ErrSignatureMismatch = errors.New("dir: filename signature does not match meta vclock sum")

	// ErrDuplicate is returned by OpenDir when two indexed files share the
	// same VClock signature. The directory is structurally ambiguous in
	// that case — two files claim to start at the same Lamport position.
	ErrDuplicate = errors.New("dir: two files share the same vclock signature")

	// ErrIncomparable is returned by OpenDir when adjacent entries (sorted
	// by signature) have VClocks that are not partially-ordered (i.e.
	// VClock.Compare returns ok=false). This mirrors xdir_index_file's
	// strictness — a Tarantool-produced single-instance chain must be
	// totally ordered on Compare.
	ErrIncomparable = errors.New("dir: adjacent files have incomparable vclocks")

	// ErrNoMeta is returned when a journal file's reader yields no Meta
	// (the file header could not be parsed into a usable meta block).
	ErrNoMeta = errors.New("has no meta")
)

// Dir is an immutable in-memory index of journal files for one filetype
// inside one directory. Build via OpenDir; use Files / LocateVClock /
// LocateLSN to query. Not safe for concurrent mutation — but read-only
// queries (Files / LocateVClock / LocateLSN) on a constructed Dir are safe
// to call from multiple goroutines because the underlying slice and maps
// are never mutated after construction.
type Dir struct {
	path     string
	filetype format.Filetype
	entries  []FileEntry // Sorted by Signature ascending; no duplicate signatures.
}

// OpenDir scans path for files matching <digits><ext> where ext corresponds
// to filetype. For each matching file it reads the meta header via
// reader.Open, validates that the filename signature equals the in-meta
// VClock.Signature(), and assembles a FileEntry.
//
// Behaviour:
//
//   - Files with the literal suffix `.inprogress` are skipped — they
//     are still being written and are not part of the indexed chain.
//   - Files whose basename does not match `<digits><ext>` are skipped (a
//     directory may legitimately contain README, .git, etc.).
//   - Signature mismatch → ErrSignatureMismatch wrapping path + values.
//   - Two indexed files with the same Signature → ErrDuplicate.
//   - Adjacent entries whose VClock pair is incomparable → ErrIncomparable
//     (mirrors xdir_index_file's per-pair Lamport order assertion).
//
// On success the returned Dir contains the entries sorted ascending by
// Signature (which is also ascending by VClock.Signature()).
func OpenDir(path string, filetype format.Filetype) (*Dir, error) {
	ext, err := filetype.Ext()
	if err != nil {
		return nil, fmt.Errorf("dir: ext: %w", err)
	}

	dents, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("dir: read %q: %w", path, err)
	}

	entries := make([]FileEntry, 0, len(dents))
	for _, de := range dents {
		if de.IsDir() {
			continue
		}

		name := de.Name()

		sig, ok, err := parseJournalName(name, ext)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		entry, err := loadEntry(filepath.Join(path, name), sig, filetype)
		if err != nil {
			return nil, err
		}

		entries = append(entries, entry)
	}

	return buildDir(path, filetype, entries)
}

// OpenDirFS is OpenDir over an io/fs.FS. Dir is a slash-separated fs path
// (use "." for the FS root). It enables indexing a journal directory held in an
// embed.FS, fstest.MapFS, or archive filesystem. Validation is identical to
// OpenDir (signature match, duplicate-signature and incomparable-vclock
// rejection); only the listing and opening go through fsys.
func OpenDirFS(fsys fs.FS, dir string, filetype format.Filetype) (*Dir, error) {
	ext, err := filetype.Ext()
	if err != nil {
		return nil, fmt.Errorf("dir: ext: %w", err)
	}

	dents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("dir: read %q: %w", dir, err)
	}

	entries := make([]FileEntry, 0, len(dents))
	for _, de := range dents {
		if de.IsDir() {
			continue
		}

		name := de.Name()

		sig, ok, err := parseJournalName(name, ext)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		entry, err := loadEntryFS(fsys, path.Join(dir, name), sig, filetype)
		if err != nil {
			return nil, err
		}

		entries = append(entries, entry)
	}

	return buildDir(dir, filetype, entries)
}

// parseJournalName matches a directory entry name against the <digits><ext>
// journal convention. It returns ok=false for names that are not journal files
// (in-flight .inprogress, wrong extension, non-numeric stem) — those are
// skipped silently — and an error only for a name that looks like a journal
// file but whose stem does not parse as an int64.
func parseJournalName(name, ext string) (int64, bool, error) {
	// Skip Tarantool's in-flight files entirely. They are not part of
	// the indexed chain until Writer.Close strips the suffix.
	if strings.HasSuffix(name, ".inprogress") {
		return 0, false, nil
	}

	if !strings.HasSuffix(name, ext) {
		return 0, false, nil
	}

	stem := strings.TrimSuffix(name, ext)
	if stem == "" || !allDigits(stem) {
		// Not a journal file by name — a directory may legitimately contain
		// unrelated files.
		return 0, false, nil
	}

	sig, err := strconv.ParseInt(stem, 10, 64)
	if err != nil {
		// AllDigits should have prevented this, but be explicit.
		return 0, false, fmt.Errorf("dir: parse signature from %q: %w", name, err)
	}

	return sig, true, nil
}

// buildDir sorts the assembled entries by signature and applies the structural
// invariants (no duplicate signatures, adjacent vclocks comparable), then wraps
// them in a Dir. Shared by OpenDir and OpenDirFS.
func buildDir(displayPath string, filetype format.Filetype, entries []FileEntry) (*Dir, error) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Signature < entries[j].Signature
	})

	// Reject duplicate signatures up-front. Two files claiming the same
	// vclock-sum is structurally ambiguous: a caller asking "which file
	// contains LSN X?" has no unique answer.
	for i := 1; i < len(entries); i++ {
		if entries[i].Signature == entries[i-1].Signature {
			return nil, fmt.Errorf("%w: %q and %q both at signature %d",
				ErrDuplicate, entries[i-1].Path, entries[i].Path, entries[i].Signature)
		}
	}

	// Lamport-order assertion across adjacent pairs. By Signature ascending,
	// the only acceptable Compare results are 0 (equal — already rejected
	// above as duplicate signature) and -1 (strictly less). An incomparable
	// pair means the chain diverged on multiple replicas in inconsistent
	// directions — Tarantool's xdir_index_file rejects this for a single-
	// instance chain, and so do we.
	for i := 1; i < len(entries); i++ {
		if _, ok := entries[i-1].VClock.Compare(entries[i].VClock); !ok {
			return nil, fmt.Errorf("%w: %q vs %q (%s vs %s)",
				ErrIncomparable, entries[i-1].Path, entries[i].Path,
				entries[i-1].VClock, entries[i].VClock)
		}
	}

	return &Dir{
		path:     displayPath,
		filetype: filetype,
		entries:  entries,
	}, nil
}

// loadEntry opens the file at fullPath, reads its meta header, and produces
// a FileEntry — including the filename-signature check.
func loadEntry(fullPath string, sigFromName int64, filetype format.Filetype) (FileEntry, error) {
	r, err := reader.Open(fullPath)
	if err != nil {
		return FileEntry{}, fmt.Errorf("dir: open %q: %w", fullPath, err)
	}

	return loadEntryReader(r, fullPath, sigFromName, filetype)
}

// loadEntryFS is loadEntry over an fs.FS (fullPath is a slash-separated fs path).
func loadEntryFS(fsys fs.FS, fullPath string, sigFromName int64, filetype format.Filetype) (FileEntry, error) {
	r, err := reader.OpenFS(fsys, fullPath)
	if err != nil {
		return FileEntry{}, fmt.Errorf("dir: open %q: %w", fullPath, err)
	}

	return loadEntryReader(r, fullPath, sigFromName, filetype)
}

// loadEntryReader assembles a FileEntry from an already-open reader (which it
// owns and closes), applying the filename-signature check. Shared by the OS and FS
// loaders so the meta handling lives in exactly one place.
func loadEntryReader(r *reader.Reader, fullPath string, sigFromName int64, filetype format.Filetype) (FileEntry, error) {
	// We only need the meta; close immediately to keep fd usage bounded
	// across large directories. The reader does not buffer beyond the
	// in-memory Meta after Open's eager parse.
	meta := r.Meta()
	if meta == nil {
		_ = r.Close()

		return FileEntry{}, fmt.Errorf("dir: %q %w", fullPath, ErrNoMeta)
	}
	// Snapshot VClock/PrevVClock before closing the reader — Meta itself
	// is owned by the reader, but the VClock maps are independent values
	// (format.Meta exposes them as plain map fields). We do not deep-clone
	// here; the reader will be garbage-collected and we hold the only live
	// references to these maps.
	vclock := meta.VClock
	prev := meta.PrevVClock

	// Filename signature must equal in-meta VClock.Signature().
	if vclock.Signature() != sigFromName {
		_ = r.Close()

		return FileEntry{}, fmt.Errorf("%w: %q: filename=%d, meta=%d",
			ErrSignatureMismatch, fullPath, sigFromName, vclock.Signature())
	}

	err := r.Close()
	if err != nil {
		return FileEntry{}, fmt.Errorf("dir: close %q: %w", fullPath, err)
	}

	return FileEntry{
		Path:       fullPath,
		Signature:  sigFromName,
		VClock:     vclock,
		PrevVClock: prev,
		Filetype:   filetype,
	}, nil
}

// allDigits reports whether s is a non-empty string of ASCII decimal digits.
// We use this instead of relying on strconv.ParseInt's error to distinguish
// "doesn't look like a journal file" (skip silently) from "looks like one
// but the integer is bogus" (return an error).
func allDigits(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}

// Files returns the indexed entries sorted by Signature ascending. The
// returned slice aliases the Dir's internal storage; callers must not
// mutate it. (Dir is shared-read-only by convention.)
func (d *Dir) Files() []FileEntry {
	return d.entries
}

// LocateVClock returns the entry whose [VClock, NextVClock) interval
// contains target. "Contains" uses the partial order:
//
//   - entry.VClock ≤ target (Compare returns 0 or -1) AND
//   - next entry's VClock > target (or this is the last entry).
//
// If any per-pair comparison along the way is incomparable (Compare
// ok=false), we fall back to the signature axis: pick the entry with the
// largest Signature ≤ target.Signature(). This is correct for files
// produced by a single Tarantool instance (where the full vclock and its
// signature carry the same information) but loses precision for diverged
// vclocks.
//
// Returns ErrNotFound when the directory is empty, or when target is
// strictly below every indexed entry's VClock.
func (d *Dir) LocateVClock(target format.VClock) (*FileEntry, error) {
	if len(d.entries) == 0 {
		return nil, ErrNotFound
	}

	// Walk forward; the file that contains `target` is the latest entry
	// whose VClock ≤ target. When we hit an entry whose VClock > target,
	// we stop and return the previous one.
	var bestIdx = -1

	usedSignatureFallback := false

	for i := range d.entries {
		ord, ok := d.entries[i].VClock.Compare(target)
		if !ok {
			// Incomparable — fall back to signature axis for this entry.
			usedSignatureFallback = true

			if d.entries[i].Signature <= target.Signature() {
				bestIdx = i

				continue
			}
			// Entry's signature is above target's; assume "after target"
			// and stop. (Crude but documented in the doc comment.)
			break
		}

		if ord <= 0 {
			// Entry.VClock ≤ target → this entry is a candidate; keep it
			// and look for a later one that is still ≤ target.
			bestIdx = i

			continue
		}
		// Ord > 0 → entry.VClock > target. No later entry can contain
		// target (entries are sorted ascending), so stop.
		break
	}

	if bestIdx < 0 {
		return nil, ErrNotFound
	}

	_ = usedSignatureFallback // Reserved for future debug instrumentation.

	return &d.entries[bestIdx], nil
}

// LocateLSN projects all indexed VClocks onto a single replica axis
// (replicaID) and returns the entry with the largest VClock[replicaID]
// such that VClock[replicaID] ≤ lsn. This is the file whose tx stream
// includes the given LSN for the given replica.
//
// Missing replicaID in an entry's VClock is treated as 0 (consistent with
// VClock.Signature / Compare semantics).
//
// Returns ErrNotFound when the directory is empty or when lsn is strictly
// below every indexed VClock[replicaID].
func (d *Dir) LocateLSN(replicaID uint32, lsn int64) (*FileEntry, error) {
	if len(d.entries) == 0 {
		return nil, ErrNotFound
	}

	bestIdx := -1

	var bestLSN int64

	for i := range d.entries {
		v := d.entries[i].VClock[replicaID]
		if v > lsn {
			continue
		}

		if bestIdx < 0 || v >= bestLSN {
			bestIdx = i
			bestLSN = v
		}
	}

	if bestIdx < 0 {
		return nil, ErrNotFound
	}

	return &d.entries[bestIdx], nil
}
