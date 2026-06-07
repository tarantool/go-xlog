//go:build !unix

package reader

import (
	"fmt"
	"os"
)

// OpenMmap falls back to reading the whole file into memory and constructing an
// in-memory Reader (NewReaderBytes) on platforms without unix mmap. Callers get
// the same zero-copy block slicing over the preloaded buffer, just without the
// demand-paged mapping. The returned Reader owns nothing extra; Close is a no-op
// for I/O (the buffer is GC-managed).
func OpenMmap(path string, opts ...Option) (*Reader, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reader: read %q: %w", path, err)
	}

	return newReaderBytes(b, opts...)
}
