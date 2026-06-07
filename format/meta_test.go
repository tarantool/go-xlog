package format_test

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
)

// TestDecodeMeta_PopulatedSnap checks the well-known fixture.
// Populated.snap has VClock {1: 12} per testdata/README.md.
func TestDecodeMeta_PopulatedSnap(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "populated.snap")
	r := bufio.NewReader(bytes.NewReader(data))
	m, err := format.DecodeMeta(r, format.MetaOptions{})
	require.NoError(t, err, "DecodeMeta")
	assert.Equalf(t, format.FiletypeSNAP, m.Filetype, "Filetype: got %q want %q", m.Filetype, format.FiletypeSNAP)
	assert.Equalf(t, "0.13", m.FormatVer, "FormatVer: got %q want 0.13", m.FormatVer)
	assert.NotEqual(t, uuid.Nil, m.InstanceUUID, "InstanceUUID is nil")
	assert.Equalf(t, int64(12), m.VClock[1], "VClock[1]: got %d want 12", m.VClock[1])
}

// TestDecodeMeta_SimpleXlog checks the simple.xlog fixture: VClock is empty
// (pre-bootstrap), so the file embeds `VClock: {}` and we expect an empty
// (non-nil) map after parsing.
func TestDecodeMeta_SimpleXlog(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	r := bufio.NewReader(bytes.NewReader(data))
	m, err := format.DecodeMeta(r, format.MetaOptions{})
	require.NoError(t, err, "DecodeMeta")
	assert.Equalf(t, format.FiletypeXLOG, m.Filetype, "Filetype: got %q want XLOG", m.Filetype)
	assert.Emptyf(t, m.VClock, "VClock: got %v want empty", m.VClock)
}

// TestDecodeMeta_VyLog checks the vylog fixture.
func TestDecodeMeta_VyLog(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "vylog_sample.vylog")
	r := bufio.NewReader(bytes.NewReader(data))
	m, err := format.DecodeMeta(r, format.MetaOptions{})
	require.NoError(t, err, "DecodeMeta")
	assert.Equalf(t, format.FiletypeVYLOG, m.Filetype, "Filetype: got %q want VYLOG", m.Filetype)
}

// TestEncodeMeta_RoundTrip ensures EncodeMeta → DecodeMeta preserves
// every populated field.
func TestEncodeMeta_RoundTrip(t *testing.T) {
	t.Parallel()

	in := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "3.8.0-test",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		VClock:       format.VClock{1: 42, 2: 7},
		PrevVClock:   format.VClock{1: 41, 2: 6},
		Extras:       map[string]string{"Custom": "value"},
	}

	var buf bytes.Buffer
	require.NoError(t, format.EncodeMeta(&buf, in), "EncodeMeta")
	r := bufio.NewReader(&buf)
	out, err := format.DecodeMeta(r, format.MetaOptions{})
	require.NoError(t, err, "DecodeMeta")
	assert.Truef(t, out.Filetype == in.Filetype && out.FormatVer == format.FormatVersion &&
		out.Version == in.Version && out.InstanceUUID == in.InstanceUUID,
		"scalar mismatch: in=%+v out=%+v", in, out)
	assert.Truef(t, equalVClock(out.VClock, in.VClock), "VClock: got %v want %v", out.VClock, in.VClock)
	assert.Truef(t, equalVClock(out.PrevVClock, in.PrevVClock), "PrevVClock: got %v want %v", out.PrevVClock, in.PrevVClock)
	assert.Equalf(t, "value", out.Extras["Custom"], "Extras: got %v", out.Extras)
}

// TestEncodeMeta_FormatVer covers the format-version line: empty FormatVer
// defaults to "0.13", a valid legacy "0.12" is honored, and an unknown
// version is rejected with ErrMetaBadVersion.
func TestEncodeMeta_FormatVer(t *testing.T) {
	t.Parallel()

	base := func() *format.Meta {
		return &format.Meta{
			Filetype:     format.FiletypeXLOG,
			Version:      "v",
			InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		}
	}

	t.Run("empty defaults to 0.13", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		require.NoError(t, format.EncodeMeta(&buf, base()))
		assert.Contains(t, buf.String(), "XLOG\n0.13\n")
	})

	t.Run("honors legacy 0.12", func(t *testing.T) {
		t.Parallel()

		m := base()
		m.FormatVer = format.LegacyFormatVersion

		var buf bytes.Buffer
		require.NoError(t, format.EncodeMeta(&buf, m))
		assert.Contains(t, buf.String(), "XLOG\n0.12\n")
	})

	t.Run("rejects unknown version", func(t *testing.T) {
		t.Parallel()

		m := base()
		m.FormatVer = "9.99"

		var buf bytes.Buffer
		require.ErrorIs(t, format.EncodeMeta(&buf, m), format.ErrMetaBadVersion)
	})
}

// TestEncodeMeta_EmptyVClockEmitted — the VClock line is always written
// (empty renders "VClock: {}", required by Tarantool's signature check),
// while an empty PrevVClock line is omitted.
func TestEncodeMeta_EmptyVClockEmitted(t *testing.T) {
	t.Parallel()

	m := &format.Meta{
		Filetype:     format.FiletypeSNAP,
		Version:      "v",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
	}

	var buf bytes.Buffer
	require.NoError(t, format.EncodeMeta(&buf, m))
	s := buf.String()
	assert.Containsf(t, s, "VClock: {}\n", "EncodeMeta must emit an empty VClock line; output:\n%s", s)
	assert.NotContainsf(t, s, "PrevVClock:", "EncodeMeta should omit PrevVClock when empty; output:\n%s", s)
}

// TestDecodeMeta_RejectsV012WhenNotEnabled — "0.12" is rejected unless AcceptV012 is set.
func TestDecodeMeta_RejectsV012WhenNotEnabled(t *testing.T) {
	t.Parallel()

	const meta = "XLOG\n0.12\nVersion: x\nInstance: 11111111-2222-3333-4444-555555555555\n\n"

	r := bufio.NewReader(bytes.NewReader([]byte(meta)))
	_, err := format.DecodeMeta(r, format.MetaOptions{})
	require.ErrorIs(t, err, format.ErrMetaBadVersion)
}

// TestDecodeMeta_AcceptV012 — opt-in legacy acceptance.
func TestDecodeMeta_AcceptV012(t *testing.T) {
	t.Parallel()

	const meta = "XLOG\n0.12\nVersion: x\nInstance: 11111111-2222-3333-4444-555555555555\n\n"

	r := bufio.NewReader(bytes.NewReader([]byte(meta)))
	m, err := format.DecodeMeta(r, format.MetaOptions{AcceptV012: true})
	require.NoError(t, err, "DecodeMeta")
	assert.Equalf(t, "0.12", m.FormatVer, "FormatVer: got %q want 0.12", m.FormatVer)
}

// TestDecodeMeta_ServerAlias — legacy "Server:" header is treated as
// "Instance:".
func TestDecodeMeta_ServerAlias(t *testing.T) {
	t.Parallel()

	const meta = "XLOG\n0.13\nVersion: x\nServer: 11111111-2222-3333-4444-555555555555\n\n"

	r := bufio.NewReader(bytes.NewReader([]byte(meta)))
	m, err := format.DecodeMeta(r, format.MetaOptions{})
	require.NoError(t, err, "DecodeMeta")
	assert.Equalf(t, "11111111-2222-3333-4444-555555555555", m.InstanceUUID.String(), "InstanceUUID via Server alias: got %s", m.InstanceUUID)
}
