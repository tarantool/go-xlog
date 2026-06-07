package follow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// engine is the shared state machine behind every follower. It emits one
// format.XRow at a time via next; the tx iterators group those rows into
// transactions in the follow layer (see runTx).
//
// The loop re-opens the current file at e.off each time it must wait for more
// bytes or move to the next file, so consumed blocks are never re-read. e.off is
// a clean block boundary (reader.Offset), captured before each read so a
// truncated tail resumes at the start of the incomplete unit (at-least-once),
// never skipping it. ErrTruncated and a still-missing EOF marker are handled
// internally (wait + retry); next only ever returns a row, io.EOF (single-file
// follow reaching the finalised end), ctx.Err(), or a real decode/I/O error.
type engine struct {
	cfg config

	r   *reader.Reader // Current open reader; nil between opens.
	off int64          // Resume offset for the current file.

	// File mode.
	fileMode bool
	path     string // Current file path being read.

	// Directory mode.
	dirPath   string
	filetype  format.Filetype
	started   bool          // Whether the start file has been resolved.
	curSig    int64         // Signature of the current file (successor must exceed it).
	curVClock format.VClock // VClock of the current file (successor must chain onto it).

	// In-progress tailing (WithReadInprogress).
	isInprogress bool
	ipSig        int64         // Signature parsed from the .inprogress filename.
	ipVClock     format.VClock // VClock read from the .inprogress meta, captured on first open.
}

// next returns the next row, blocking (subject to ctx) for appended data or a
// rotated file. It returns io.EOF only when a single-file follow reaches the
// finalised end; a directory follow never returns io.EOF (it waits for the next
// file). ctx cancellation surfaces as ctx.Err().
func (e *engine) next(ctx context.Context) (format.XRow, error) {
	if !e.fileMode && !e.started {
		if err := e.startDir(ctx); err != nil {
			return format.XRow{}, err
		}

		e.started = true
	}

	for {
		if err := ctx.Err(); err != nil {
			return format.XRow{}, fmt.Errorf("follow: %w", err)
		}

		if e.r == nil {
			if err := e.open(); err != nil {
				return format.XRow{}, err
			}
		}

		row, err := e.r.Next()
		switch {
		case err == nil:
			e.off = e.r.Offset()

			return row, nil
		case errors.Is(err, io.EOF):
			cont, err := e.onEOF(ctx)
			if err != nil {
				return format.XRow{}, err
			}

			if !cont {
				return format.XRow{}, io.EOF
			}
		case errors.Is(err, reader.ErrTruncated):
			// Partial trailing block: the previous block is already drained, so
			// Offset points at the start of the incomplete block — resume there
			// to re-read it in full once written.
			e.off = e.r.Offset()
			if err := e.onWait(ctx); err != nil {
				return format.XRow{}, err
			}
		default:
			_ = e.closeReader()

			return format.XRow{}, fmt.Errorf("follow: read %q: %w", e.path, err)
		}
	}
}

// onEOF handles a clean end-of-stream. It returns cont=true to continue the read
// loop (waited / switched files) or cont=false to stop with io.EOF (a finalised
// single-file follow).
func (e *engine) onEOF(ctx context.Context) (bool, error) {
	finalized := e.r.SawEOFMarker()
	e.off = e.r.Offset()
	_ = e.closeReader()

	if e.fileMode {
		if finalized {
			return false, nil
		}

		return true, e.wait(ctx, e.path)
	}

	// Directory mode.
	if !e.isInprogress {
		if finalized {
			return true, e.advance(ctx)
		}
		// A finalised, index-visible file always carries its marker; reaching
		// here means it does not yet — wait for it to be written.
		return true, e.wait(ctx, e.path)
	}

	return true, e.onInprogressEOF(ctx, finalized)
}

// onInprogressEOF handles end-of-stream while tailing a live .inprogress file.
func (e *engine) onInprogressEOF(ctx context.Context, finalized bool) error {
	if finalized {
		// Caught the EOF marker just before the writer renamed the file. The
		// current file is complete; record its identity and advance.
		e.adoptInprogressIdentity()

		return e.advance(ctx)
	}

	// Not finalised: either the writer paused, or it finalised by renaming the
	// .inprogress to its final name (so our path vanished). If the final file
	// now exists, re-resolve onto it and continue from the same offset.
	if e.reswitchToFinalized() {
		return nil
	}

	return e.wait(ctx, e.path)
}

// onWait is the truncated-tail counterpart to onEOF's wait: it closes the reader
// and handles the .inprogress→final rename race before waiting.
func (e *engine) onWait(ctx context.Context) error {
	_ = e.closeReader()

	if !e.fileMode && e.isInprogress && e.reswitchToFinalized() {
		return nil
	}

	return e.wait(ctx, e.path)
}

// wait blocks via the configured watcher, wrapping its error.
func (e *engine) wait(ctx context.Context, path string) error {
	if err := e.cfg.watcher.Wait(ctx, path); err != nil {
		return fmt.Errorf("follow: wait: %w", err)
	}

	return nil
}

// reswitchToFinalized detects that the .inprogress file we were tailing has been
// renamed to its final name and switches to reading the finalised file from the
// same offset. Returns true when it switched.
func (e *engine) reswitchToFinalized() bool {
	finalPath := strings.TrimSuffix(e.path, ".inprogress")
	if finalPath == e.path {
		return false
	}

	if fileExists(e.path) || !fileExists(finalPath) {
		return false
	}

	e.adoptInprogressIdentity()
	e.path = finalPath

	return true
}

// adoptInprogressIdentity promotes the in-progress file's parsed identity to be
// the "current" file, so the next successor lookup chains onto it.
func (e *engine) adoptInprogressIdentity() {
	e.curSig = e.ipSig
	if e.ipVClock != nil {
		e.curVClock = e.ipVClock
	}

	e.isInprogress = false
}

// open opens the current file at e.off with IgnoreMissingEOF forced on. For an
// in-progress file it captures the meta vclock (first open) and validates chain
// continuity against the previous file.
func (e *engine) open() error {
	opts := make([]reader.Option, 0, len(e.cfg.readerOpts)+1)
	opts = append(opts, reader.IgnoreMissingEOF())
	opts = append(opts, e.cfg.readerOpts...)

	r, err := reader.OpenAt(e.path, e.off, opts...)
	if err != nil {
		return fmt.Errorf("follow: open %q: %w", e.path, err)
	}

	if e.isInprogress {
		if e.ipVClock == nil {
			e.ipVClock = r.Meta().VClock.Clone()
		}

		if !vclockEqual(r.Meta().PrevVClock, e.curVClock) {
			_ = r.Close()

			return fmt.Errorf("%w: %q prev=%s, expected %s",
				ErrChainBroken, e.path, r.Meta().PrevVClock, e.curVClock)
		}
	}

	e.r = r

	return nil
}

// startDir resolves the directory follow's first file per the start option.
func (e *engine) startDir(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("follow: %w", err)
		}

		d, err := dir.OpenDir(e.dirPath, e.filetype)
		if err != nil {
			return fmt.Errorf("follow: index %q: %w", e.dirPath, err)
		}

		files := d.Files()

		switch e.cfg.mode {
		case startHead:
			if len(files) == 0 {
				// Empty directory — wait for the first file to appear.
				if err := e.wait(ctx, e.dirPath); err != nil {
					return err
				}

				continue
			}

			e.setCurrent(files[0])

			return nil
		case startLSN:
			entry, err := d.LocateLSN(e.cfg.lsnReplica, e.cfg.lsn)
			if err != nil {
				return fmt.Errorf("follow: locate lsn: %w", err)
			}

			e.setCurrent(*entry)

			return nil
		case startVClock:
			entry, err := d.LocateVClock(e.cfg.vclock)
			if err != nil {
				return fmt.Errorf("follow: locate vclock: %w", err)
			}

			e.setCurrent(*entry)

			return nil
		case startNone:
			return ErrNoStart
		default:
			return ErrNoStart
		}
	}
}

// advance moves the directory follow to the next file in the chain once the
// current one is finalised, blocking (subject to ctx) until a successor appears.
func (e *engine) advance(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("follow: %w", err)
		}

		next, found, err := e.findSuccessor()
		if err != nil {
			return err
		}

		if found {
			e.setCurrent(next)

			return nil
		}

		if e.cfg.readInprogress {
			ipPath, ipSig, ok, err := e.findInprogressSuccessor()
			if err != nil {
				return err
			}

			if ok {
				e.path = ipPath
				e.ipSig = ipSig
				e.isInprogress = true
				e.ipVClock = nil
				e.off = 0

				return nil
			}
		}

		if err := e.wait(ctx, e.dirPath); err != nil {
			return err
		}
	}
}

// findSuccessor re-indexes the directory and returns the finalised file with the
// smallest signature greater than the current one, validating that it chains
// onto the current file. found is false when no successor exists yet.
func (e *engine) findSuccessor() (dir.FileEntry, bool, error) {
	d, err := dir.OpenDir(e.dirPath, e.filetype)
	if err != nil {
		return dir.FileEntry{}, false, fmt.Errorf("follow: index %q: %w", e.dirPath, err)
	}

	files := d.Files()

	best := -1

	for i := range files {
		if files[i].Signature <= e.curSig {
			continue
		}

		if best < 0 || files[i].Signature < files[best].Signature {
			best = i
		}
	}

	if best < 0 {
		return dir.FileEntry{}, false, nil
	}

	if !vclockEqual(files[best].PrevVClock, e.curVClock) {
		return dir.FileEntry{}, false, fmt.Errorf("%w: %q prev=%s, expected %s",
			ErrChainBroken, files[best].Path, files[best].PrevVClock, e.curVClock)
	}

	return files[best], true, nil
}

// findInprogressSuccessor scans for an active <digits><ext>.inprogress file with
// signature greater than the current one and returns the smallest such.
func (e *engine) findInprogressSuccessor() (string, int64, bool, error) {
	ext, err := e.filetype.Ext()
	if err != nil {
		return "", 0, false, fmt.Errorf("follow: ext: %w", err)
	}

	suffix := ext + ".inprogress"

	dents, err := os.ReadDir(e.dirPath)
	if err != nil {
		return "", 0, false, fmt.Errorf("follow: read %q: %w", e.dirPath, err)
	}

	bestSig := int64(-1)

	var bestName string

	for _, de := range dents {
		if de.IsDir() {
			continue
		}

		stem, ok := strings.CutSuffix(de.Name(), suffix)
		if !ok || !allDigits(stem) {
			continue
		}

		sig, err := strconv.ParseInt(stem, 10, 64)
		if err != nil || sig <= e.curSig {
			continue
		}

		if bestSig < 0 || sig < bestSig {
			bestSig = sig
			bestName = de.Name()
		}
	}

	if bestSig < 0 {
		return "", 0, false, nil
	}

	return filepath.Join(e.dirPath, bestName), bestSig, true, nil
}

// setCurrent points the engine at a finalised entry as its current file.
func (e *engine) setCurrent(entry dir.FileEntry) {
	e.curSig = entry.Signature
	e.curVClock = entry.VClock
	e.path = entry.Path
	e.isInprogress = false
	e.ipVClock = nil
	e.off = 0
}

// closeReader closes and clears the current reader, if any.
func (e *engine) closeReader() error {
	if e.r == nil {
		return nil
	}

	err := e.r.Close()
	e.r = nil

	if err != nil {
		return fmt.Errorf("follow: close: %w", err)
	}

	return nil
}

// run drives the row iterator. ctx cancellation and io.EOF end iteration
// cleanly (no error yielded).
func (e *engine) run(ctx context.Context, yield func(format.XRow, error) bool) {
	defer func() { _ = e.closeReader() }()

	for {
		row, err := e.next(ctx)
		if err != nil {
			if stopClean(err) {
				return
			}

			yield(format.XRow{}, err)

			return
		}

		if !yield(row, nil) {
			return
		}
	}
}

// runTx drives the transaction iterator: it groups the row stream into logical
// transactions (a maximal run sharing a tsn, terminated by IsCommit), the same
// grouping reader.NextTx applies. A single-file follow that ends mid-tx (rows
// pending at the finalised EOF) yields reader.ErrTruncated, matching NextTx.
func (e *engine) runTx(ctx context.Context, yield func(*reader.Transaction, error) bool) {
	defer func() { _ = e.closeReader() }()

	var rows []format.XRow

	for {
		row, err := e.next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) && len(rows) > 0 {
				yield(nil, reader.ErrTruncated)

				return
			}

			if stopClean(err) {
				return
			}

			yield(nil, err)

			return
		}

		rows = append(rows, row)
		if row.IsCommit() {
			if !yield(&reader.Transaction{Rows: rows, StartLSN: rows[0].LSN}, nil) {
				return
			}

			rows = nil
		}
	}
}

// stopClean reports whether err is one of the clean terminators (finalised EOF
// or context cancellation) that end iteration without yielding an error.
func stopClean(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// fileExists reports whether path currently exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// allDigits reports whether s is a non-empty run of ASCII decimal digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}
