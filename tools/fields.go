package tools

import (
	"github.com/google/uuid"

	"github.com/tarantool/go-xlog/format"
)

// Option overrides a single meta header field in an on-copy rewrite performed
// by RewriteMetaFields. Options follow the functional-options pattern (cf.
// Writer.Option): each mutates the cloned meta in place, and any field
// without a corresponding Option is left exactly as the source had it.
type Option func(*format.Meta)

// WithFormatVer sets the format-version line. It must be "0.13" or the legacy
// "0.12" — EncodeMeta rejects anything else. Changing the version does NOT
// re-encode tx rows (they are copied verbatim), so only retarget the
// version when the existing row encoding is already compatible.
func WithFormatVer(v string) Option {
	return func(m *format.Meta) { m.FormatVer = v }
}

// WithInstanceUUID sets the instance UUID.
func WithInstanceUUID(id uuid.UUID) Option {
	return func(m *format.Meta) { m.InstanceUUID = id }
}

// WithVersion sets the free-form Version string (Tarantool's PACKAGE_VERSION).
func WithVersion(v string) Option {
	return func(m *format.Meta) { m.Version = v }
}

// WithVClock sets the VClock line. A copy of vc is stored, so later mutation
// of vc by the caller does not affect the rewrite. As with RemapVClock this
// touches only the header: a vclock that disagrees with the per-row LSN sums
// will fail Tarantool's signature check at load time.
func WithVClock(vc format.VClock) Option {
	return func(m *format.Meta) { m.VClock = vc.Clone() }
}

// RewriteMetaFields copies srcPath to dstPath, overwriting the header fields
// named by opts and preserving every tx block and the EOF marker verbatim.
// It is a thin convenience over RewriteMeta: listed fields change,
// everything else (PrevVClock, Extras, and any field without an Option) is
// kept. With no opts it is a meta-preserving copy.
//
// See RewriteMeta for the failure modes and the verbatim-tx-bytes contract.
func RewriteMetaFields(srcPath, dstPath string, opts ...Option) error {
	return RewriteMeta(srcPath, dstPath, func(m *format.Meta) *format.Meta {
		for _, opt := range opts {
			opt(m)
		}

		return m
	})
}
