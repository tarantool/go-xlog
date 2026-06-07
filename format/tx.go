package format

import (
	"errors"
	"fmt"

	"github.com/tarantool/go-iproto"
)

// TxOptions controls EncodeTxBlock / AppendTxBlock behaviour.
type TxOptions struct {
	// Compression is the block compression policy (zstd level + threshold, or
	// disabled). The zero value compresses at ZstdLevel over payloads of at
	// least CompressThreshold bytes (Tarantool's behaviour); set Disabled for
	// byte-inspectable plain output, or Level/Threshold to tune.
	Compression Compression
}

// ErrCorruptCRC is returned by DecodeTxBlock when the computed CRC32C does
// not match the value in the fixheader.
var ErrCorruptCRC = errors.New("format: tx block: crc32c mismatch")

// Sentinel errors for tx-block decoding.
var (
	ErrEOFMarkerBlock = errors.New("EOF marker, not a tx block")
	ErrMissingType    = errors.New("header missing IPROTO_REQUEST_TYPE")
)

// EncodeTxBlock encodes rows into a complete on-disk tx block:
// fixheader + payload. The payload is each row's EncodeXRow output
// concatenated in order. If opts.Compression says to compress (the payload
// meets its threshold and it is not Disabled), the payload is zstd-compressed
// and the fixheader magic is ZRowMarker; otherwise RowMarker is used. The CRC32C
// in the fixheader is computed over the on-disk payload bytes
// (post-compression for zrow, plain for row) per src/box/xlog.c:1086,1165.
func EncodeTxBlock(rows []XRow, opts TxOptions) ([]byte, error) {
	return AppendTxBlock(nil, rows, opts)
}

// AppendTxBlockPayload appends the concatenated EncodeXRow output of rows —
// the *uncompressed* tx payload, with no fixheader — to dst and returns the
// extended buffer. Passing a reused dst[:0] makes the per-tx payload encode
// allocation-free in steady state (the buffer grows to the largest tx, then
// reuses its capacity). It is the building block BatchWriter accumulates into,
// and the first half of AppendTxBlock.
func AppendTxBlockPayload(dst []byte, rows []XRow) ([]byte, error) {
	for i := range rows {
		var err error

		dst, err = EncodeXRow(dst, &rows[i])
		if err != nil {
			return nil, fmt.Errorf("format: AppendTxBlockPayload: row %d: %w", i, err)
		}
	}

	return dst, nil
}

// AppendTxBlock appends a complete on-disk tx block (fixheader + payload) for
// rows to dst and returns the extended buffer. Compression and CRC follow the
// opts.Compression policy (zstd at its level iff the payload meets the policy's
// threshold and it is not Disabled). EncodeTxBlock is AppendTxBlock(nil, …);
// writers reuse a dst across txs to avoid the per-block allocation.
//
// Note: the payload is encoded into a transient buffer internally, so this
// path still allocates that scratch. The fully reusable hot path lives in the
// writer package (AppendTxBlockPayload into owned scratch + framing), which is
// what high-throughput writers should use.
func AppendTxBlock(dst []byte, rows []XRow, opts TxOptions) ([]byte, error) {
	plain, err := AppendTxBlockPayload(nil, rows)
	if err != nil {
		return nil, err
	}

	return AppendFramedBlock(dst, plain, opts.Compression)
}

// AppendFramedBlock frames an already-encoded plain payload into a complete
// on-disk tx block (fixheader + payload) appended to dst, returning the
// extended buffer. It compresses the payload (into a pooled scratch) when the
// Compression policy says to, computes the CRC32C over the on-disk bytes,
// and writes the fixheader. Callers holding their own reusable scratch
// should prefer CompressTxInto + EncodeFixheader directly.
func AppendFramedBlock(dst, plain []byte, c Compression) ([]byte, error) {
	var (
		payload []byte
		magic   [4]byte
	)

	if c.Compresses(len(plain)) {
		compressed, err := CompressTxInto(nil, plain, c.ResolvedLevel())
		if err != nil {
			return nil, fmt.Errorf("format: AppendFramedBlock: compress: %w", err)
		}

		payload = compressed
		magic = ZRowMarker
	} else {
		payload = plain
		magic = RowMarker
	}

	crc := CRC32C(payload)
	h := &Fixheader{
		Magic:  magic,
		Len:    uint32(len(payload)), //nolint:gosec // G115: tx payload length is a uint32 fixheader field
		CRC32P: 0,                    // Always 0, not validated on read.
		CRC32C: crc,
	}

	var fhBuf [FixheaderSize]byte
	EncodeFixheader(&fhBuf, h)

	dst = append(dst, fhBuf[:]...)
	dst = append(dst, payload...)

	return dst, nil
}

// DecodeTxBlock parses a complete tx block at b[0:] and returns:
//   - rowSlices: byte slices, one per xrow record, into the *plain* payload
//   - plain:     the decompressed (or as-is) payload bytes
//   - n:         total bytes consumed from b (fixheader + on-disk payload)
//   - err:       on CRC mismatch, malformed header, or truncated input
//
// rowSlices aliases plain — callers that retain row slices past plain's
// lifetime must clone them. The plain slice may be a fresh allocation
// (for zrow blocks) or alias the input (for row blocks).
func DecodeTxBlock(b []byte) ([][]byte, []byte, int, error) {
	if len(b) < FixheaderSize {
		return nil, nil, 0, fmt.Errorf("%w: need %d bytes for fixheader, have %d", ErrShortFixheader, FixheaderSize, len(b))
	}

	var fh [FixheaderSize]byte
	copy(fh[:], b[:FixheaderSize])

	h, err := DecodeFixheader(fh)
	if err != nil {
		return nil, nil, 0, err
	}

	if h.Magic == EOFMarker {
		return nil, nil, 0, fmt.Errorf("format: DecodeTxBlock: %w", ErrEOFMarkerBlock)
	}

	if int(h.Len) > len(b)-FixheaderSize {
		return nil, nil, 0, fmt.Errorf("%w: payload len=%d exceeds remaining %d", ErrShortFixheader, h.Len, len(b)-FixheaderSize)
	}

	onDisk := b[FixheaderSize : FixheaderSize+int(h.Len)]

	// CRC over the on-disk bytes.
	if got := CRC32C(onDisk); got != h.CRC32C {
		return nil, nil, 0, fmt.Errorf("%w: have 0x%08x, want 0x%08x", ErrCorruptCRC, got, h.CRC32C)
	}

	var plain []byte
	if h.Magic == ZRowMarker {
		plain, err = DecompressTx(onDisk, nil)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("format: DecodeTxBlock: %w", err)
		}
	} else { // RowMarker.
		plain = onDisk
	}

	// Walk the plain payload, recording per-row byte ranges.
	rowSlices, err := splitRows(plain)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("format: DecodeTxBlock: split rows: %w", err)
	}

	return rowSlices, plain, FixheaderSize + int(h.Len), nil
}

// splitRows walks a plain payload, slicing it into per-xrow []byte ranges.
// Each row consists of:
//
//   - a msgpack map (the header) — always present, parsed enough to peek
//     IPROTO_REQUEST_TYPE so we can decide whether a body follows;
//   - if Type != IPROTO_NOP and there are bytes remaining: one msgpack value
//     (the body map).
//
// We do NOT call DecodeXRow here — we only need to know byte boundaries.
// This separation lets the caller (or test) decode each row separately and
// also lets row-level filters skip body decoding entirely.
func splitRows(payload []byte) ([][]byte, error) {
	var out [][]byte

	off := 0
	for off < len(payload) {
		start := off
		// Header: a msgpack map.
		entries, hdrPrefix, err := readMPMapLen(payload[off:])
		if err != nil {
			return nil, fmt.Errorf("row header at offset %d: %w", off, err)
		}

		hdrStart := off + hdrPrefix
		// Walk header entries, tracking type.
		var (
			rowType iproto.Type
			sawType bool
		)

		cur := hdrStart
		for i := range entries {
			key, n, err := readMPUint(payload[cur:])
			if err != nil {
				return nil, fmt.Errorf("row header key %d at offset %d: %w", i, cur, err)
			}

			cur += n
			if iproto.Key(key) == iproto.IPROTO_REQUEST_TYPE { //nolint:gosec // G115: msgpack header key is a small protocol number
				t, n2, err := readMPUint(payload[cur:])
				if err != nil {
					return nil, fmt.Errorf("row header type at offset %d: %w", cur, err)
				}

				rowType = iproto.Type(t) //nolint:gosec // G115: request type is a small enum bounded by iproto.Type
				sawType = true
				cur += n2
			} else {
				n2, err := skipMP(payload[cur:])
				if err != nil {
					return nil, fmt.Errorf("row header value at offset %d: %w", cur, err)
				}

				cur += n2
			}
		}

		if !sawType {
			return nil, fmt.Errorf("row at offset %d: %w", start, ErrMissingType)
		}

		off = cur
		// Body: NOP rows have no body. Other rows have one msgpack value if
		// any bytes remain (Tarantool always emits a body for non-NOP rows,
		// but we tolerate absence to keep splitRows liberal).
		if rowType != iproto.IPROTO_NOP && off < len(payload) {
			n, err := skipMP(payload[off:])
			if err != nil {
				return nil, fmt.Errorf("row body at offset %d: %w", off, err)
			}

			off += n
		}

		out = append(out, payload[start:off])
	}

	return out, nil
}
