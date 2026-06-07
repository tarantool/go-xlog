package tools

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/tarantool/go-xlog/format"
)

// Sentinel errors returned by ReplaceInstanceUUIDInPlace.
var (
	// ErrUUIDLineNotFound is returned when the header has no Instance/Server
	// line to overwrite.
	ErrUUIDLineNotFound = errors.New("tools.ReplaceInstanceUUIDInPlace: no Instance/Server line in header")

	// ErrUUIDWidthMismatch is returned when the on-disk UUID value is not the
	// same byte width as the canonical replacement, so an in-place overwrite
	// would shift following bytes. Use RewriteMetaFields (on-copy) instead.
	ErrUUIDWidthMismatch = errors.New("tools.ReplaceInstanceUUIDInPlace: on-disk UUID width differs from replacement; use RewriteMetaFields")

	// ErrCorruptUUIDBackup is returned by RecoverInstanceUUIDInPlace when the
	// undo-log sidecar exists but is too short or self-inconsistent to apply.
	ErrCorruptUUIDBackup = errors.New("tools.RecoverInstanceUUIDInPlace: corrupt undo-log sidecar")
)

// uuidBackupSuffix is appended to the target path to name the undo-log
// sidecar that ReplaceInstanceUUIDInPlace writes before its in-place
// overwrite. Its presence after a crash signals an interrupted operation.
const uuidBackupSuffix = ".uuidbak"

// uuidBackupHeaderLen is the fixed prefix of the sidecar: an 8-byte
// big-endian file offset, followed by the original value bytes.
const uuidBackupHeaderLen = 8

// maxUUIDBackupValueLen bounds the original-value payload of a sidecar so a
// malformed file cannot drive an absurd WriteAt during recovery. A canonical
// UUID is 36 bytes; the cap leaves generous headroom.
const maxUUIDBackupValueLen = 64

// testHookAfterBackup, if non-nil, is invoked by ReplaceInstanceUUIDInPlace
// immediately after the undo-log sidecar has been written and synced but
// before the in-place WriteAt. Tests use it to simulate a crash inside the
// torn-write window. It is always nil in production builds.
var testHookAfterBackup func(path string, valStart, valEnd int)

// ReplaceInstanceUUIDInPlace overwrites the instance UUID in path's meta
// header directly, without copying the file. This is sound only because a
// canonical UUID is always exactly 36 bytes, so the replacement occupies the
// same span and no following byte moves — tx blocks, their CRCs, and the EOF
// marker are left untouched. Use it to re-stamp the instance UUID of a
// large file without rewriting every byte; for any other header change, or
// when the on-disk UUID is not 36 bytes, use RewriteMetaFields.
//
// Crash safety. The 36-byte WriteAt is not itself atomic: a power loss or
// kernel crash while the span straddles a sector boundary can leave a torn,
// unparseable UUID. To make this recoverable without giving up the no-copy
// property, the original bytes and their offset are first written to an
// undo-log sidecar (path+".uuidbak"), fsync'd, then the in-place write is
// performed and fsync'd, and only then is the sidecar removed. If the process
// dies in that window the sidecar survives; the next call to
// ReplaceInstanceUUIDInPlace heals it automatically (it runs recovery on
// entry), or the caller can run RecoverInstanceUUIDInPlace explicitly. The
// net guarantee is old-or-new, never a torn header.
//
// The file is opened read-write. It returns ErrUUIDLineNotFound if the header
// has no Instance/Server line, ErrUUIDWidthMismatch if the on-disk value is
// not 36 bytes, and leaves the file unmodified in both cases. When the
// on-disk UUID already equals newID the call is a no-op. Legacy 0.12 headers
// are accepted (the version is not touched).
func ReplaceInstanceUUIDInPlace(path string, newID uuid.UUID) error {
	// Heal any sidecar left by a previously interrupted call before touching
	// the header: it rolls the file back to a consistent (old-UUID) state so
	// the parse below sees a sane header rather than a torn one.
	if _, err := RecoverInstanceUUIDInPlace(path); err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: recover stale backup: %w", err)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: open %q: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	// Validate it is a real header and locate where the header ends. The
	// bufio reader prefetches past the terminator, so the true header length
	// is bytes-consumed minus bytes-still-buffered.
	counting := &countingReader{r: f}
	br := bufio.NewReader(counting)

	if _, err := format.DecodeMeta(br, format.MetaOptions{AcceptV012: true}); err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: decode meta from %q: %w", path, err)
	}

	headerLen := counting.n - int64(br.Buffered())

	// Re-read the header region to find the exact byte span of the UUID value.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: seek: %w", err)
	}

	header := make([]byte, headerLen)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: read header: %w", err)
	}

	valStart, valEnd, ok := instanceValueSpan(header)
	if !ok {
		return ErrUUIDLineNotFound
	}

	want := newID.String() // Canonical 36-byte 8-4-4-4-12 form.
	if valEnd-valStart != len(want) {
		return fmt.Errorf("%w: on-disk %d bytes, replacement %d bytes", ErrUUIDWidthMismatch, valEnd-valStart, len(want))
	}

	// Already the requested value: nothing to write, no sidecar churn.
	if bytes.Equal(header[valStart:valEnd], []byte(want)) {
		return nil
	}

	// Undo log: persist the original bytes (and their offset) so a crash in
	// the WriteAt window below is recoverable. Written and fsync'd before the
	// destructive write.
	if err := writeUUIDBackup(path, int64(valStart), header[valStart:valEnd]); err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: write backup: %w", err)
	}

	if testHookAfterBackup != nil {
		testHookAfterBackup(path, valStart, valEnd)
	}

	if _, err := f.WriteAt([]byte(want), int64(valStart)); err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: write: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: sync: %w", err)
	}

	// New value is durable; the undo log is no longer needed.
	if err := removeUUIDBackup(path); err != nil {
		return fmt.Errorf("tools.ReplaceInstanceUUIDInPlace: remove backup: %w", err)
	}

	return nil
}

// RecoverInstanceUUIDInPlace completes the crash-recovery half of
// ReplaceInstanceUUIDInPlace. If path has an undo-log sidecar
// (path+".uuidbak"), a prior in-place overwrite was interrupted: this
// restores the original bytes recorded in the sidecar to their offset in
// path (so a torn UUID becomes the consistent pre-write value again) and
// then removes the sidecar. It reports whether a recovery was performed.
//
// With no sidecar present it is a no-op returning (false, nil), so it is safe
// to call unconditionally before opening a file. A sidecar that is too short
// or describes an out-of-range span is rejected with ErrCorruptUUIDBackup and
// left in place for inspection.
func RecoverInstanceUUIDInPlace(path string) (bool, error) {
	backupPath := path + uuidBackupSuffix

	data, err := os.ReadFile(backupPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("tools.RecoverInstanceUUIDInPlace: read backup %q: %w", backupPath, err)
	}

	if len(data) <= uuidBackupHeaderLen || len(data)-uuidBackupHeaderLen > maxUUIDBackupValueLen {
		return false, fmt.Errorf("%w: %q has %d bytes", ErrCorruptUUIDBackup, backupPath, len(data))
	}

	//nolint:gosec // A wrapped value surfaces as a negative offset, rejected next.
	offset := int64(binary.BigEndian.Uint64(data[:uuidBackupHeaderLen]))
	orig := data[uuidBackupHeaderLen:]

	if offset < 0 {
		return false, fmt.Errorf("%w: negative offset %d in %q", ErrCorruptUUIDBackup, offset, backupPath)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("tools.RecoverInstanceUUIDInPlace: open %q: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.WriteAt(orig, offset); err != nil {
		return false, fmt.Errorf("tools.RecoverInstanceUUIDInPlace: restore: %w", err)
	}

	if err := f.Sync(); err != nil {
		return false, fmt.Errorf("tools.RecoverInstanceUUIDInPlace: sync: %w", err)
	}

	if err := removeUUIDBackup(path); err != nil {
		return false, fmt.Errorf("tools.RecoverInstanceUUIDInPlace: remove backup: %w", err)
	}

	return true, nil
}

// writeUUIDBackup creates the undo-log sidecar for path, recording offset and
// the original value bytes, and fsyncs both the sidecar and its directory so
// the record survives a crash that strikes during the subsequent in-place
// write.
func writeUUIDBackup(path string, offset int64, orig []byte) error {
	buf := make([]byte, uuidBackupHeaderLen+len(orig))
	//nolint:gosec // offset is a non-negative in-file position (a header value span).
	binary.BigEndian.PutUint64(buf[:uuidBackupHeaderLen], uint64(offset))
	copy(buf[uuidBackupHeaderLen:], orig)

	backupPath := path + uuidBackupSuffix

	bf, err := os.OpenFile(backupPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, inprogressFilePerm)
	if err != nil {
		return fmt.Errorf("open %q: %w", backupPath, err)
	}

	if _, err := bf.Write(buf); err != nil {
		_ = bf.Close()

		return fmt.Errorf("write %q: %w", backupPath, err)
	}

	if err := bf.Sync(); err != nil {
		_ = bf.Close()

		return fmt.Errorf("sync %q: %w", backupPath, err)
	}

	if err := bf.Close(); err != nil {
		return fmt.Errorf("close %q: %w", backupPath, err)
	}

	// Fsync the directory so the sidecar's existence is itself durable.
	if err := syncDir(filepath.Dir(backupPath)); err != nil {
		return fmt.Errorf("sync dir of %q: %w", backupPath, err)
	}

	return nil
}

// removeUUIDBackup deletes the undo-log sidecar for path and fsyncs the
// directory so the removal is durable. A missing sidecar is not an error.
func removeUUIDBackup(path string) error {
	backupPath := path + uuidBackupSuffix

	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %q: %w", backupPath, err)
	}

	if err := syncDir(filepath.Dir(backupPath)); err != nil {
		return fmt.Errorf("sync dir of %q: %w", backupPath, err)
	}

	return nil
}

// instanceValueSpan returns the byte span [start, end) of the UUID value on
// the "Instance:" (or legacy "Server:") line within header, excluding the
// key, the colon, surrounding spaces, and the trailing CR/newline. The bool
// is false when no such line exists.
func instanceValueSpan(header []byte) (int, int, bool) {
	for lineStart := 0; lineStart < len(header); {
		nl := bytes.IndexByte(header[lineStart:], '\n')
		if nl < 0 {
			break
		}

		lineEnd := lineStart + nl
		line := header[lineStart:lineEnd]

		var keyLen int

		switch {
		case bytes.HasPrefix(line, []byte("Instance:")):
			keyLen = len("Instance:")
		case bytes.HasPrefix(line, []byte("Server:")):
			keyLen = len("Server:")
		default:
			lineStart = lineEnd + 1

			continue
		}

		valStart := lineStart + keyLen
		valEnd := lineEnd

		for valStart < valEnd && header[valStart] == ' ' {
			valStart++
		}

		for valEnd > valStart && (header[valEnd-1] == ' ' || header[valEnd-1] == '\r') {
			valEnd--
		}

		return valStart, valEnd, true
	}

	return 0, 0, false
}
