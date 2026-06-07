// Package checkpoint holds the logic shared by the cat and play example
// commands: collecting .xlog/.snap files, parsing the --timestamp value, and
// composing the row filter both commands apply.
//
// The filter is assembled from the core go-xlog `filter` package. The
// predicates defined here — including the body-aware SpaceIn and
// ExcludeSystemSpaces — demonstrate how a module *outside* the core library
// can supply its own filter.Filter values: the core ships only the
// header-field predicates plus the And/Or/Not combinators, and anything that
// needs the row body (decoded with the dependency-free format.DecodeDMLBody)
// lives in a downstream package like this one. They all compose with
// filter.And exactly like the built-in predicates.
package checkpoint

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/tarantool/go-xlog/filter"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// systemSpaceMax is the first user space id; ids below it are system spaces.
const systemSpaceMax = 512

// Opts are the shared filter inputs of cat and play. Defaults: From=0,
// To=MaxUint64, Timestamp=+Inf — i.e. "keep everything".
type Opts struct {
	From       uint64
	To         uint64
	Timestamp  float64
	Spaces     []int
	Replicas   []int
	ShowSystem bool
}

// DefaultOpts returns Opts with the "keep everything" defaults.
func DefaultOpts() Opts {
	return Opts{To: math.MaxUint64, Timestamp: math.Inf(1)}
}

// ParseTimestamp parses the --timestamp value. An empty string means "no
// upper bound" (+Inf). Otherwise it accepts either fractional Unix seconds
// (e.g. 1731592956.818) or an RFC3339 / RFC3339Nano instant, matching tt.
func ParseTimestamp(s string) (float64, error) {
	if s == "" {
		return math.Inf(1), nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, fmt.Errorf("invalid --timestamp %q: %w", s, err)
	}
	return float64(t.UnixNano()) / 1e9, nil
}

// Filter composes the cat/play row filter from opts using the core filter
// combinators. Predicates are ANDed; the body-aware space predicate is placed
// last so the cheaper header predicates short-circuit first and rows they
// reject never pay the body decode.
func Filter(opts Opts) filter.Filter {
	preds := []filter.Filter{
		LSNFrom(opts.From),
		LSNUntil(opts.To),
		UntilTimestamp(opts.Timestamp),
	}
	if len(opts.Replicas) > 0 {
		preds = append(preds, filter.ReplicaIDs(toUint32(opts.Replicas)...))
	}
	switch {
	case len(opts.Spaces) > 0:
		preds = append(preds, SpaceIn(toUint32(opts.Spaces)...))
	case !opts.ShowSystem:
		preds = append(preds, ExcludeSystemSpaces())
	}
	return filter.And(preds...)
}

// LSNFrom keeps rows with LSN >= from. Header-only.
func LSNFrom(from uint64) filter.Filter {
	return func(r format.XRow) bool { return uint64(r.LSN) >= from } //nolint:gosec // xlog LSNs are non-negative.
}

// LSNUntil keeps rows with LSN < to (upper bound exclusive, matching tt).
func LSNUntil(to uint64) filter.Filter {
	return func(r format.XRow) bool { return uint64(r.LSN) < to } //nolint:gosec // xlog LSNs are non-negative.
}

// UntilTimestamp keeps rows whose timestamp is < ts (exclusive).
func UntilTimestamp(ts float64) filter.Filter {
	return func(r format.XRow) bool { return r.Timestamp < ts }
}

// SpaceIn keeps DML rows whose space id is in the set; rows without a space
// (NOP/RAFT/…) are dropped. Body-aware: it decodes the space id via the pure
// format.DecodeDMLBody, so it needs no external dependency.
func SpaceIn(ids ...uint32) filter.Filter {
	set := make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return func(r format.XRow) bool {
		sid, ok := spaceID(r)
		if !ok {
			return false
		}
		_, ok = set[sid]
		return ok
	}
}

// ExcludeSystemSpaces drops DML rows in Tarantool's system spaces (id < 512).
// Rows without a space (NOP/RAFT/…) pass through. Body-aware.
func ExcludeSystemSpaces() filter.Filter {
	return func(r format.XRow) bool {
		sid, ok := spaceID(r)
		if !ok {
			return true
		}
		return sid >= systemSpaceMax
	}
}

// spaceID returns the row's DML space id, or ok=false when the row carries no
// space (no body, undecodable, or space_id 0).
func spaceID(r format.XRow) (uint32, bool) {
	if len(r.BodyRaw) == 0 {
		return 0, false
	}
	b, err := format.DecodeDMLBody(r.BodyRaw)
	if err != nil || b.SpaceID == 0 {
		return 0, false
	}
	return b.SpaceID, true
}

func toUint32(in []int) []uint32 {
	out := make([]uint32, len(in))
	for i, v := range in {
		out[i] = uint32(v) //nolint:gosec // space/replica ids fit in uint32.
	}
	return out
}

// CollectFiles expands the given paths into a list of .xlog/.snap files. A
// path to a file is taken as-is; a directory is scanned for journal files
// (recursively when recursive is true). The result is de-duplicated and the
// files contributed by each path are sorted by name, which for Tarantool's
// zero-padded LSN filenames is also chronological order.
func CollectFiles(paths []string, recursive bool) ([]string, error) {
	var out []string
	seen := map[string]struct{}{}

	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", p, err)
		}
		if !info.IsDir() {
			add(p)
			continue
		}

		var found []string
		walk := func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if !recursive && path != p {
					return fs.SkipDir
				}
				return nil
			}
			if isJournal(path) {
				found = append(found, path)
			}
			return nil
		}
		if err := filepath.WalkDir(p, walk); err != nil {
			return nil, fmt.Errorf("scan %q: %w", p, err)
		}
		sort.Strings(found)
		for _, f := range found {
			add(f)
		}
	}

	return out, nil
}

func isJournal(path string) bool {
	switch filepath.Ext(path) {
	case ".xlog", ".snap":
		return true
	default:
		return false
	}
}

// Record is one filtered row handed to the caller's callback. Body is the
// decoded DML body, or nil for rows without one (e.g. NOP).
type Record struct {
	File string
	Row  format.XRow
	Body *format.DMLBody
}

// Process opens each file in turn and invokes fn for every row the keep filter
// accepts.
func Process(files []string, keep filter.Filter, fn func(Record) error) error {
	for _, file := range files {
		if err := processFile(file, keep, fn); err != nil {
			return err
		}
	}
	return nil
}

func processFile(file string, keep filter.Filter, fn func(Record) error) error {
	r, err := reader.Open(file)
	if err != nil {
		return fmt.Errorf("open %q: %w", file, err)
	}
	defer func() { _ = r.Close() }()

	for row, err := range r.Rows() {
		if err != nil {
			return fmt.Errorf("read %q: %w", file, err)
		}
		if !keep(row) {
			continue
		}

		var body *format.DMLBody
		if len(row.BodyRaw) > 0 {
			if b, derr := format.DecodeDMLBody(row.BodyRaw); derr == nil {
				body = b
			}
		}
		if err := fn(Record{File: file, Row: row, Body: body}); err != nil {
			return err
		}
	}
	return nil
}
