package tools

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/durable"
)

// Sentinel errors returned by RewriteMeta.
var (
	errNilTransformFn = errors.New("tools.RewriteMeta: nil transform fn")
	errNilMetaFromFn  = errors.New("tools.RewriteMeta: transform fn returned nil meta")
)

// syncDir fsyncs the directory holding dstPath so the `.inprogress` → final
// rename survives power loss. A package var (defaulting to durable.SyncDir) so
// tests can observe that RewriteMeta issues the directory sync.
var syncDir = durable.SyncDir

// inprogressFilePerm is the mode for the .inprogress staging file (owner
// read/write only); it is replaced via atomic rename on success.
const inprogressFilePerm = 0o600

// countingReader wraps an io.Reader and tracks the total number of bytes
// successfully delivered to callers. Used to compute the meta-end offset
// of a source file: after bufio-fed DecodeMeta returns, the meta ends at
// `total bytes consumed from source - bytes still buffered in bufio`.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	//nolint:wrapcheck // io.Reader contract: EOF/err passed through unchanged.
	return n, err
}

// RewriteMeta reads srcPath, parses its meta header, hands a deep clone of
// that meta to fn (so the caller's transform cannot mutate state shared
// with the on-disk parse), encodes the returned meta into a fresh
// `<dstPath>.inprogress` file, byte-copies every byte of srcPath from the
// position right after the source's meta blank-line terminator through to
// the end of the file (including the 4-byte EOF marker), syncs, and
// atomically renames `.inprogress` → dstPath.
//
// Tx blocks are NOT re-encoded — their CRCs and zstd-compressed payloads
// are preserved exactly. The caller-supplied transform is therefore
// expected to limit its changes to header fields; any rewrite that would
// invalidate the rows (e.g. a VClock signature that disagrees with the
// per-row LSN sums) will be rejected by Tarantool at load time.
//
// Failure modes:
//
//   - srcPath cannot be opened or its meta is malformed: error returned;
//     no `.inprogress` file is created.
//   - The destination `.inprogress` already exists (O_EXCL): error
//     returned; nothing is overwritten.
//   - Any failure after the destination is created: the partial
//     `.inprogress` file is removed before returning.
//
// On success, dstPath exists and any pre-existing file at dstPath is
// replaced atomically via os.Rename.
func RewriteMeta(srcPath, dstPath string, fn func(*format.Meta) *format.Meta) error {
	if fn == nil {
		return errNilTransformFn
	}

	// --- Stage 1: parse source meta and compute its end offset. ---.
	srcParseF, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("tools.RewriteMeta: open src %q: %w", srcPath, err)
	}
	// Defer this close; we open a second handle for the byte-copy stage.
	// Best-effort: srcParseF is read-only and fully consumed by DecodeMeta.
	defer func() { _ = srcParseF.Close() }()

	counting := &countingReader{r: srcParseF}
	br := bufio.NewReader(counting)

	meta, err := format.DecodeMeta(br, format.MetaOptions{})
	if err != nil {
		return fmt.Errorf("tools.RewriteMeta: decode meta from %q: %w", srcPath, err)
	}
	// After DecodeMeta, bufio has prefetched bytes past the blank line.
	// The "true" position of the next unread byte = counting.n - br.Buffered().
	metaEndOffset := counting.n - int64(br.Buffered())

	// --- Stage 2: open dst.inprogress and write the transformed meta. ---.
	inprogress := dstPath + ".inprogress"

	dstF, err := os.OpenFile(inprogress, os.O_CREATE|os.O_EXCL|os.O_WRONLY, inprogressFilePerm)
	if err != nil {
		return fmt.Errorf("tools.RewriteMeta: open dst %q: %w", inprogress, err)
	}
	// Any failure from here through Rename must clean up the partial file.
	committed := false

	defer func() {
		if !committed {
			_ = os.Remove(inprogress)
		}
	}()

	newMeta := fn(meta.Clone())
	if newMeta == nil {
		// A nil transform output is unambiguous garbage; fail loudly
		// rather than guessing what to write.
		_ = dstF.Close()

		return errNilMetaFromFn
	}

	if err := format.EncodeMeta(dstF, newMeta); err != nil {
		_ = dstF.Close()

		return fmt.Errorf("tools.RewriteMeta: encode meta: %w", err)
	}

	// --- Stage 3: byte-copy source tail (tx blocks + EOF marker). ---
	// Open a fresh handle and seek to the meta-end offset rather than
	// trying to drain the bufio.Reader (the bufio has prefetched into
	// memory; we'd have to manually emit those buffered bytes first then
	// io.Copy the rest — opening twice is cleaner).
	srcCopyF, err := os.Open(srcPath)
	if err != nil {
		_ = dstF.Close()

		return fmt.Errorf("tools.RewriteMeta: reopen src %q: %w", srcPath, err)
	}
	// Best-effort: srcCopyF is read-only and fully consumed by io.Copy.
	defer func() { _ = srcCopyF.Close() }()

	if _, err := srcCopyF.Seek(metaEndOffset, io.SeekStart); err != nil {
		_ = dstF.Close()

		return fmt.Errorf("tools.RewriteMeta: seek src to %d: %w", metaEndOffset, err)
	}

	if _, err := io.Copy(dstF, srcCopyF); err != nil {
		_ = dstF.Close()

		return fmt.Errorf("tools.RewriteMeta: copy tx bytes: %w", err)
	}

	// --- Stage 4: sync, close, atomic rename. ---.
	if err := dstF.Sync(); err != nil {
		_ = dstF.Close()

		return fmt.Errorf("tools.RewriteMeta: sync dst: %w", err)
	}

	if err := dstF.Close(); err != nil {
		return fmt.Errorf("tools.RewriteMeta: close dst: %w", err)
	}

	if err := os.Rename(inprogress, dstPath); err != nil {
		return fmt.Errorf("tools.RewriteMeta: rename %q -> %q: %w", inprogress, dstPath, err)
	}

	committed = true

	// fsync the parent directory so the rename is durable: syncing dstF (above)
	// flushes the file's bytes but not the directory entry that publishes the
	// final name. Without this a power loss could leave the rewritten file
	// stranded as .inprogress or missing despite RewriteMeta returning success.
	if err := syncDir(filepath.Dir(dstPath)); err != nil {
		return fmt.Errorf("tools.RewriteMeta: sync dir: %w", err)
	}

	return nil
}

// ReplaceInstanceUUID returns a transform fn that sets m.InstanceUUID to
// newID and leaves every other field untouched. Suitable as the fn
// argument to RewriteMeta.
func ReplaceInstanceUUID(newID uuid.UUID) func(*format.Meta) *format.Meta {
	return func(m *format.Meta) *format.Meta {
		m.InstanceUUID = newID

		return m
	}
}

// RemapVClock returns a transform fn that renumbers replica ids in both
// VClock and PrevVClock according to remap: a key oldID present in remap
// becomes remap[oldID]; a key not present in remap is left at its current
// id. Per-row ReplicaID values inside the tx blocks are NOT rewritten by
// this transform — RewriteMeta copies tx bytes verbatim. Callers
// who need a consistent rewrite (so Tarantool's filename-signature check
// stays valid) must also remap row ReplicaIDs by other means; otherwise
// the resulting file's vclock signature will not match the sum of the
// rows' LSNs that load into Tarantool.
func RemapVClock(remap map[uint32]uint32) func(*format.Meta) *format.Meta {
	return func(m *format.Meta) *format.Meta {
		m.VClock = remapVC(m.VClock, remap)
		m.PrevVClock = remapVC(m.PrevVClock, remap)

		return m
	}
}

// remapVC renumbers v according to remap. Missing keys in remap pass through
// unchanged. Returns nil if v is nil (preserves "no vclock line" semantics
// in EncodeMeta).
func remapVC(v format.VClock, remap map[uint32]uint32) format.VClock {
	if v == nil {
		return nil
	}

	out := make(format.VClock, len(v))
	for oldID, lsn := range v {
		newID, ok := remap[oldID]
		if !ok {
			newID = oldID
		}

		out[newID] = lsn
	}

	return out
}
