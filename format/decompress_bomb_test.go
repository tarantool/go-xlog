package format_test

import (
	"encoding/binary"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
)

// zstdBombFrame hand-crafts a minimal, structurally valid zstd frame whose
// header *declares* a content size of declaredSize bytes while carrying a
// single empty raw block — i.e. a decompression bomb: a ~20-byte payload that
// instructs the decoder to materialise declaredSize bytes. A decoder without a
// memory cap allocates declaredSize up front (klauspost: decoder.go pre-sizes
// the destination from FrameContentSize) before it ever inspects the data.
//
// Layout per the zstd RFC (8878):
//   - 4-byte magic 0xFD2FB528 (little-endian on disk)
//   - 1-byte Frame_Header_Descriptor: Single_Segment_flag set + FCS field size 8
//   - 8-byte Frame_Content_Size (little-endian)
//   - one block header: Last_Block=1, Block_Type=Raw(0), Block_Size=0
func zstdBombFrame(declaredSize uint64) []byte {
	frame := make([]byte, 0, 4+1+8+3)
	frame = append(frame, 0x28, 0xB5, 0x2F, 0xFD) // magic

	// Frame_Header_Descriptor 0xE0 = 0b1110_0000:
	//   bits 7-6 = 11 -> Frame_Content_Size_flag 3 -> 8-byte FCS field,
	//   bit 5    = 1  -> Single_Segment_flag (no Window_Descriptor, window=FCS).
	frame = append(frame, 0xE0)

	var fcs [8]byte
	binary.LittleEndian.PutUint64(fcs[:], declaredSize)
	frame = append(frame, fcs[:]...)

	// Block header (3-byte little-endian): Last_Block(bit0)=1, Block_Type(bits
	// 2-1)=0 (Raw), Block_Size(bits 23-3)=0 -> value 0x000001.
	frame = append(frame, 0x01, 0x00, 0x00)

	return frame
}

// TestDecompressTx_RejectsDecompressionBomb is the regression test for the
// zstd decompression-bomb hole: DecompressTx must refuse a frame that declares
// more than MaxDecompressedTxSize bytes, returning zstd.ErrDecoderSizeExceeded
// instead of attempting the giant allocation. The declared size is set one byte
// over the cap so the un-capped path's allocation (and this test's footprint)
// stays bounded while still proving the guard fires from the declared size.
func TestDecompressTx_RejectsDecompressionBomb(t *testing.T) {
	t.Parallel()

	bomb := zstdBombFrame(format.MaxDecompressedTxSize + 1)

	out, err := format.DecompressTx(bomb, nil)
	require.ErrorIs(t, err, zstd.ErrDecoderSizeExceeded,
		"must be rejected for exceeding the decoded-size cap, not decoded then errored")
	assert.Nil(t, out, "no bytes returned on rejection")
}

// TestDecodeTxBlock_RejectsDecompressionBomb proves the guard holds on the
// reader-facing path: a well-formed tx block (valid fixheader + CRC) whose
// ZRowMarker payload is a decompression bomb is rejected without OOM.
func TestDecodeTxBlock_RejectsDecompressionBomb(t *testing.T) {
	t.Parallel()

	payload := zstdBombFrame(format.MaxDecompressedTxSize + 1)

	// Frame the bomb as a genuine on-disk ZRowMarker tx block: the CRC is
	// computed over the (compressed) on-disk bytes, exactly as the writer does,
	// so decoding reaches the decompression step rather than failing on CRC.
	h := &format.Fixheader{
		Magic:  format.ZRowMarker,
		Len:    uint32(len(payload)),
		CRC32C: format.CRC32C(payload),
	}

	var fhBuf [format.FixheaderSize]byte
	format.EncodeFixheader(&fhBuf, h)

	block := append(fhBuf[:], payload...)

	rows, plain, n, err := format.DecodeTxBlock(block)
	require.ErrorIs(t, err, zstd.ErrDecoderSizeExceeded,
		"tx block carrying a decompression bomb must be rejected")
	assert.Nil(t, rows)
	assert.Nil(t, plain)
	assert.Zero(t, n)
}

// TestDecompressTx_AllowsAtCap is the negative control: a legitimate payload
// that decompresses to exactly the cap must still round-trip. This pins the
// boundary so the bomb guard cannot be "fixed" by clamping below real data.
func TestDecompressTx_AllowsAtCap(t *testing.T) {
	t.Parallel()

	// Highly compressible payload sized at the cap: small on disk, exactly
	// MaxDecompressedTxSize when expanded.
	plain := make([]byte, format.MaxDecompressedTxSize)

	compressed, err := format.CompressTx(plain)
	require.NoError(t, err)

	out, err := format.DecompressTx(compressed, nil)
	require.NoError(t, err, "a payload exactly at the cap must decompress")
	assert.Len(t, out, format.MaxDecompressedTxSize)
}
