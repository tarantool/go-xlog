//go:build unix

package reader

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// OpenMmap memory-maps path read-only and returns a Reader that slices tx
// blocks directly out of the mapping — no read syscalls and no per-block copy
// in the steady state. It is the path-shaped counterpart to NewReaderBytes: the
// mapped bytes are the Reader's buffer, so uncompressed (RowMarker) blocks
// decode zero-copy and, with WithAliasBodies, the row bodies alias the mapping
// and stay valid until Close. ZRow blocks still decompress.
//
// The returned Reader owns the mapping and the file descriptor; Close munmaps
// and closes the fd. The caller must not use any retained aliasing body after
// Close (the mapping is gone). Available on unix platforms only; other platforms
// have a NewReaderBytes-backed fallback.
func OpenMmap(path string, opts ...Option) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reader: open %q: %w", path, err)
	}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("reader: stat %q: %w", path, err)
	}

	size := fi.Size()
	if size <= 0 {
		// Nothing to map; route through the in-memory path so a 0-byte file
		// yields the same clean meta-decode error a NewReaderBytes(nil) would.
		_ = f.Close()

		return newReaderBytes(nil, opts...)
	}

	//nolint:gosec // G115: a file descriptor and a file size both fit in int on the unix targets we map on.
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("reader: mmap %q: %w", path, err)
	}

	r, err := newReaderBytes(data, opts...)
	if err != nil {
		_ = syscall.Munmap(data)
		_ = f.Close()

		return nil, err
	}

	r.owned = &mmapCloser{data: data, f: f}

	return r, nil
}

// mmapCloser releases a mapping and its file descriptor, surfacing the first
// error. It satisfies io.Closer so it slots into Reader.owned.
type mmapCloser struct {
	data []byte
	f    *os.File
}

// Close munmaps the region and closes the fd, returning the first failure.
func (m *mmapCloser) Close() error {
	munmapErr := syscall.Munmap(m.data)
	closeErr := m.f.Close()

	if err := errors.Join(munmapErr, closeErr); err != nil {
		return fmt.Errorf("reader: close mmap: %w", err)
	}

	return nil
}
