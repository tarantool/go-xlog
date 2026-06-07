// Package follow tails Tarantool journal files as they are written. It adds
// two live-read modes on top of the forward-only reader:
//
//   - FollowFile / FollowFileTx — tail a single .xlog, blocking for rows the
//     writer appends, until the file is finalised (its EOF marker appears) or
//     the context is cancelled.
//   - FollowDir / FollowDirTx — tail a whole WAL directory: read the current
//     file, and when it is finalised switch to the next file in the rotation
//     chain (validating chain continuity), continuing indefinitely.
//
// Both are exposed as Go 1.23 iterators (iter.Seq2), matching reader.Rows /
// reader.Txs. Cancellation via context.Context ends the iterator cleanly
// (without yielding an error), the same way io.EOF ends the reader iterators.
//
// The mechanism is offset-tracked re-open: each poll re-opens the file at the
// byte offset a prior pass stopped at (reader.OpenAt + reader.Offset) and reads
// the newly-appended blocks, so already-consumed blocks are never re-read. A
// partial trailing block or a still-missing EOF marker means "wait for more";
// the real EOF marker (reader.SawEOFMarker) means "finalised". Directory and
// file growth are observed by a pluggable Watcher; the default polls.
package follow

import (
	"context"
	"errors"
	"time"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// defaultPollInterval is the cadence the default (polling) Watcher re-checks a
// file for growth or a directory for a new rotated file. Sub-second so a
// follower stays close to the writer without busy-spinning.
const defaultPollInterval = 200 * time.Millisecond

// Sentinel errors. Callers match with errors.Is.
var (
	// ErrChainBroken is returned when the next file in a directory does not
	// chain onto the current one — its PrevVClock does not equal the current
	// file's VClock. The rotation chain has a gap or has diverged; the follower
	// stops rather than silently skipping records.
	ErrChainBroken = errors.New("follow: chain continuity broken: successor PrevVClock != current VClock")

	// ErrNoStart is returned by FollowDir / FollowDirTx when no start option
	// was given. A directory follow has no default position — pass WithFromHead,
	// WithStartLSN, or WithStartVClock.
	ErrNoStart = errors.New("follow: directory follow requires a start option (WithFromHead/WithStartLSN/WithStartVClock)")
)

// Watcher decides when a follower re-checks the filesystem. Wait blocks until
// path may have changed (a file grew, a directory gained a file) or ctx is
// done, in which case it returns ctx.Err(). It is only a hint to re-check, not
// a guarantee of change — the follower re-reads and re-resolves regardless — so
// the default pollWatcher simply waits one poll interval. The seam lets an
// event-driven implementation (e.g. fsnotify) be slotted in via WithWatcher
// without changing the follower.
type Watcher interface {
	Wait(ctx context.Context, path string) error
}

// startMode selects how a directory follow resolves its first file.
type startMode int

const (
	startNone startMode = iota
	startHead
	startLSN
	startVClock
)

// config is the resolved per-follower configuration.
type config struct {
	pollInterval time.Duration
	readerOpts   []reader.Option
	watcher      Watcher

	// File mode: byte offset to resume from (a prior reader.Offset()).
	startOffset int64

	// Directory mode.
	mode           startMode
	lsnReplica     uint32
	lsn            int64
	vclock         format.VClock
	readInprogress bool
}

func defaultConfig() config {
	return config{pollInterval: defaultPollInterval}
}

// resolve fills in defaults that depend on other options (the poll watcher
// needs the resolved interval). Called once after all options are applied.
func (c *config) resolve() {
	if c.watcher == nil {
		c.watcher = pollWatcher{interval: c.pollInterval}
	}
}

func applyOptions(opts []Option) config {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	cfg.resolve()

	return cfg
}

// Option configures a follower. Options are applied in order.
type Option func(*config)

// WithPollInterval sets the cadence the default Watcher re-checks for new bytes
// or files. Default 200ms. Ignored when a custom Watcher is supplied.
func WithPollInterval(d time.Duration) Option {
	return func(c *config) { c.pollInterval = d }
}

// WithReaderOptions threads reader Options (e.g. SkipCorruptTx, AcceptV012,
// WithAliasBodies) into every underlying open. IgnoreMissingEOF is always
// applied internally — a follower must tolerate the not-yet-finalised tail.
func WithReaderOptions(opts ...reader.Option) Option {
	return func(c *config) { c.readerOpts = append(c.readerOpts, opts...) }
}

// WithStartOffset begins a single-file follow at a byte offset previously
// reported by reader.Offset / Follower.Offset (always a clean block boundary).
// It has no effect on a directory follow. Default 0 (start after the header).
func WithStartOffset(off int64) Option {
	return func(c *config) { c.startOffset = off }
}

// WithFromHead starts a directory follow at the lowest-signature file — i.e.
// replay the whole chain on disk, then follow live.
func WithFromHead() Option {
	return func(c *config) { c.mode = startHead }
}

// WithStartLSN starts a directory follow at the file containing (replicaID, lsn)
// via dir.LocateLSN.
func WithStartLSN(replicaID uint32, lsn int64) Option {
	return func(c *config) {
		c.mode = startLSN
		c.lsnReplica = replicaID
		c.lsn = lsn
	}
}

// WithStartVClock starts a directory follow at the file containing target via
// dir.LocateVClock.
func WithStartVClock(target format.VClock) Option {
	return func(c *config) {
		c.mode = startVClock
		c.vclock = target.Clone()
	}
}

// WithReadInprogress lets a directory follow tail the active <sig>.<ext>.inprogress
// file directly for lowest latency, instead of waiting for it to be finalised
// (renamed) before reading. Off by default: only finalised, index-visible files
// are read, which keeps chain validation simple and avoids the rename race.
func WithReadInprogress() Option {
	return func(c *config) { c.readInprogress = true }
}

// WithWatcher overrides the default polling Watcher (e.g. with an fsnotify-based
// one). The follower's correctness does not depend on the Watcher reporting
// real changes — it re-reads regardless — so a Watcher only affects latency and
// CPU, never what is emitted.
func WithWatcher(w Watcher) Option {
	return func(c *config) { c.watcher = w }
}

// vclockEqual reports whether two vclocks are equal under the partial order
// (missing replicas treated as 0).
func vclockEqual(a, b format.VClock) bool {
	ord, ok := a.Compare(b)

	return ok && ord == 0
}
