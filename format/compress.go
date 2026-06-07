package format

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Compression is a tx-block compression policy. Compression is applied at
// block granularity (a whole tx block is zstd-compressed or not — there is no
// per-row compression). The zero value means "default": zstd at ZstdLevel over
// any payload of at least CompressThreshold bytes — i.e. exactly Tarantool's
// behaviour. A writer holds one policy; EncodeTxBlock / AppendTxBlock take one.
type Compression struct {
	// Disabled turns compression off entirely: every block is written plain
	// (RowMarker) regardless of size.
	Disabled bool

	// Level is the zstd compression level. Zero means ZstdLevel (Tarantool's
	// 3). Passed to klauspost's EncoderLevelFromZstd.
	Level int

	// Threshold is the minimum plain-payload size in bytes at which a block is
	// compressed; smaller blocks are written plain. Zero means
	// CompressThreshold (2 KiB). Ignored when Disabled.
	Threshold int
}

// ResolvedLevel returns the effective zstd level, substituting ZstdLevel for a
// zero Level.
func (c Compression) ResolvedLevel() int {
	if c.Level == 0 {
		return ZstdLevel
	}

	return c.Level
}

// Compresses reports whether a plain payload of plainLen bytes is compressed
// under this policy (not Disabled and at or above the effective threshold).
func (c Compression) Compresses(plainLen int) bool {
	return !c.Disabled && plainLen >= c.resolvedThreshold()
}

// resolvedThreshold returns the effective compress threshold, substituting
// CompressThreshold for a zero Threshold.
func (c Compression) resolvedThreshold() int {
	if c.Threshold == 0 {
		return CompressThreshold
	}

	return c.Threshold
}

// MaxDecompressedTxSize caps the number of bytes a single zstd-compressed tx
// block (ZRowMarker) is allowed to decompress to. It is the decompression-bomb
// guard: a malicious or corrupt file can carry a tiny ZRowMarker payload whose
// zstd frame *declares* an enormous content size; klauspost's decoder defaults
// to a 64 GiB ceiling, so without this cap a few on-disk bytes can drive a
// multi-gigabyte allocation and OOM the process before a single row is read.
//
// The cap is deliberately generous relative to anything Tarantool writes: a tx
// block is auto-flushed at AutocommitThreshold (128 KiB), so a legitimate plain
// payload is at most that plus one trailing row, and a single row is bounded by
// the tuple-size limit (1 MiB by default, raisable). 64 MiB leaves ~500x
// headroom over the autocommit threshold and comfortably covers even
// large-tuple deployments, while bounding a hostile block to an allocation the
// process can survive. The decoder rejects an over-cap frame (returning
// zstd.ErrDecoderSizeExceeded) from its *declared* size, before allocating.
const MaxDecompressedTxSize = 64 << 20 // 64 MiB

// Encoders and decoders are expensive to construct (they allocate a window
// and a dictionary). We pool them per-process so writers and readers in
// tight loops do not pay the allocation cost on every tx. Encoders are pooled
// per zstd level (a small set in practice), so a level-tuned writer still
// amortises encoder construction.
//
// The pools are *not* shared state — they are a construction cache; each
// pooled instance is single-owner while checked out.
var (
	zstdEncPools sync.Map // int level -> *sync.Pool of *zstd.Encoder.
	zstdDecPool  = sync.Pool{New: func() any { return newZstdDecoder() }}
)

// getZstdEncoder checks out an encoder for the given level from its per-level
// pool, creating the pool lazily on first use of that level.
func getZstdEncoder(level int) *zstd.Encoder {
	p, ok := zstdEncPools.Load(level)
	if !ok {
		lvl := level
		p, _ = zstdEncPools.LoadOrStore(level, &sync.Pool{
			New: func() any { return newZstdEncoder(lvl) },
		})
	}

	return p.(*sync.Pool).Get().(*zstd.Encoder) //nolint:forcetypeassert // pool only holds *zstd.Encoder
}

// putZstdEncoder returns an encoder to its per-level pool.
func putZstdEncoder(level int, enc *zstd.Encoder) {
	if p, ok := zstdEncPools.Load(level); ok {
		p.(*sync.Pool).Put(enc) //nolint:forcetypeassert // we stored a *sync.Pool
	}
}

// newZstdEncoder builds a fresh encoder at the given zstd level. Klauspost/
// compress names its levels semantically; we pass the integer through
// EncoderLevelFromZstd for fidelity with Tarantool's ZSTD_compressBegin(ctx, n).
//
// WithLowerEncoderMem trims each pooled encoder's footprint at no measurable
// cost to throughput or ratio (BenchmarkZstdEncoderOptions: identical output
// bytes, identical MB/s) — worthwhile because the per-level pool may hold
// several encoders at once. WithWindowSize is deliberately not set: tx blocks
// are bounded by AutocommitThreshold, so the default window already covers any
// block and capping it only cost speed in the benchmark.
func newZstdEncoder(level int) *zstd.Encoder {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true),
	)
	if err != nil {
		// NewWriter with nil destination only fails on bad option; the level
		// is normalised by EncoderLevelFromZstd, so this cannot fail in practice.
		panic(fmt.Sprintf("format: zstd encoder init (level %d): %v", level, err))
	}

	return enc
}

// newZstdDecoder builds a fresh pooled decoder.
func newZstdDecoder() *zstd.Decoder {
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		// Bound the decoded size to defuse decompression bombs: a tiny
		// ZRowMarker payload can declare a multi-gigabyte frame and OOM the
		// process. See MaxDecompressedTxSize for the rationale behind the cap.
		zstd.WithDecoderMaxMemory(MaxDecompressedTxSize),
	)
	if err != nil {
		panic(fmt.Sprintf("format: zstd decoder init: %v", err))
	}

	return dec
}

// CompressTx zstd-encodes payload at ZstdLevel (3). The output is a
// self-contained zstd frame; Tarantool's libzstd decoder accepts it.
// Note that the output bytes are NOT byte-identical to libzstd's output —
// only the decompressed content is guaranteed to round-trip.
func CompressTx(payload []byte) ([]byte, error) {
	return CompressTxInto(nil, payload, ZstdLevel)
}

// CompressTxInto zstd-encodes payload at the given level, appending the frame
// to dst[:0] and returning the result (which may alias dst if it had enough
// capacity, or a fresh allocation otherwise). Pass a reused dst to amortise the
// output allocation across calls — the writer's hot path does exactly this.
func CompressTxInto(dst, payload []byte, level int) ([]byte, error) {
	enc := getZstdEncoder(level)
	defer putZstdEncoder(level, enc)

	// EncodeAll resets state internally — safe to reuse across calls.
	return enc.EncodeAll(payload, dst[:0]), nil
}

// DecompressTx zstd-decodes payload. The caller may pass a non-nil scratch
// to amortise allocations across calls; the returned slice is the
// decompressed bytes (may alias scratch if it was large enough).
func DecompressTx(payload, scratch []byte) ([]byte, error) {
	dec, ok := zstdDecPool.Get().(*zstd.Decoder)
	if !ok {
		dec = newZstdDecoder()
	}

	defer zstdDecPool.Put(dec)

	out, err := dec.DecodeAll(payload, scratch[:0])
	if err != nil {
		return nil, fmt.Errorf("format: zstd decode: %w", err)
	}

	return out, nil
}
