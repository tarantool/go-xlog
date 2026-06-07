package follow

import (
	"context"

	"github.com/tarantool/go-xlog/format"
)

// Follower is the stateful driver behind the iterators, for callers that want to
// pull rows one at a time and checkpoint progress across restarts via Offset.
// The iterator functions (File / Dir) are the simpler surface; reach for
// Follower when you need the resume offset.
//
// A Follower is not safe for concurrent use.
type Follower struct {
	e *engine
}

// NewFileFollower returns a Follower tailing a single file. See File for the
// semantics; WithStartOffset sets the initial resume point.
func NewFileFollower(path string, opts ...Option) *Follower {
	return &Follower{e: newFileEngine(path, opts)}
}

// NewDirFollower returns a Follower tailing a rotation chain. See Dir; a start
// option is required (else Next returns ErrNoStart). The start file is resolved
// lazily on the first Next so it can honour ctx.
func NewDirFollower(dirPath string, ft format.Filetype, opts ...Option) *Follower {
	return &Follower{e: newDirEngine(dirPath, ft, opts)}
}

// Next returns the next row, blocking (subject to ctx) for appended rows or a
// rotated file. It returns io.EOF when a single-file follow reaches the
// finalised end, and ctx.Err() on cancellation. A directory follow never
// returns io.EOF.
func (f *Follower) Next(ctx context.Context) (format.XRow, error) {
	return f.e.next(ctx)
}

// Offset returns the byte offset within the current file from which a fresh
// follow would resume without skipping rows — a clean block boundary. Persist
// it (alongside the current file path, via the dir chain) to resume after a
// restart with WithStartOffset.
func (f *Follower) Offset() int64 { return f.e.off }

// Close releases the underlying open file, if any.
func (f *Follower) Close() error { return f.e.closeReader() }
