package format

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/google/uuid"
)

// Meta is the parsed text header that every xlog/snap/vylog/run/index file
// begins with. The on-disk layout is:
//
//	<Filetype>\n               # XLOG | SNAP | VYLOG | RUN | INDEX
//	<FormatVer>\n              # "0.13" (or "0.12" with AcceptV012)
//	Version: <ver>\n           # Tarantool's PACKAGE_VERSION string
//	Instance: <uuid>\n         # 8-4-4-4-12 hex UUID (alias: "Server: ...")
//	VClock: {id: lsn, ...}\n   # ALWAYS present; empty renders "VClock: {}"
//	PrevVClock: {id: lsn, ...}\n# OMITTED when empty / for SNAP files
//	\n                         # blank line terminates the header
//
// Source: src/box/xlog.c:115-122, 155 (writer) and the meta_parse path.
type Meta struct {
	Filetype     Filetype
	FormatVer    string
	Version      string
	InstanceUUID uuid.UUID
	VClock       VClock
	PrevVClock   VClock
	// Extras captures any unknown `Key: value` line we did not recognise,
	// preserving it across rewrite round-trips (forward-compat catch-all).
	Extras map[string]string
}

// Clone returns a deep copy of m. The returned *Meta shares no maps with
// the receiver: VClock, PrevVClock, and Extras are independently allocated.
// A nil receiver returns nil.
//
// Used by tools.RewriteMeta to hand a mutation-safe copy to user transform
// functions without aliasing the parsed source meta.
func (m *Meta) Clone() *Meta {
	if m == nil {
		return nil
	}

	out := &Meta{
		Filetype:     m.Filetype,
		FormatVer:    m.FormatVer,
		Version:      m.Version,
		InstanceUUID: m.InstanceUUID,
		VClock:       m.VClock.Clone(),
		PrevVClock:   m.PrevVClock.Clone(),
	}
	if m.Extras != nil {
		out.Extras = make(map[string]string, len(m.Extras))
		maps.Copy(out.Extras, m.Extras)
	}

	return out
}

// MetaOptions controls strictness of DecodeMeta.
type MetaOptions struct {
	// AcceptV012 accepts the legacy "0.12" format version in addition to
	// the current "0.13". Default false: strict on FormatVersion.
	AcceptV012 bool
}

// Sentinel errors returned by DecodeMeta. The reader package wraps them
// with file context.
var (
	ErrMetaTruncated  = errors.New("format: meta header truncated before blank-line terminator")
	ErrMetaBadFormat  = errors.New("format: malformed meta header")
	ErrMetaBadVersion = errors.New("format: unsupported format version")

	ErrNilMeta       = errors.New("nil meta")
	ErrEmptyFiletype = errors.New("empty Filetype")
	ErrNilMetaReader = errors.New("nil reader")
)

// EncodeMeta writes the meta header. The format-version line is m.FormatVer
// when set (it must be "0.13" or the legacy "0.12", else ErrMetaBadVersion),
// and defaults to FormatVersion ("0.13") when m.FormatVer is empty — so the
// writer's freshly-built metas still emit 0.13, while a rewrite can
// preserve or retarget the source's version. The VClock line is always
// emitted (even "VClock: {}", required by Tarantool's signature check); an
// empty PrevVClock line is omitted. The header ends with a blank line.
func EncodeMeta(w io.Writer, m *Meta) error {
	if m == nil {
		return fmt.Errorf("format: EncodeMeta: %w", ErrNilMeta)
	}

	if m.Filetype == "" {
		return fmt.Errorf("format: EncodeMeta: %w", ErrEmptyFiletype)
	}

	ver := FormatVersion
	if m.FormatVer != "" {
		if m.FormatVer != FormatVersion && m.FormatVer != LegacyFormatVersion {
			return fmt.Errorf("format: EncodeMeta: %w: %q", ErrMetaBadVersion, m.FormatVer)
		}

		ver = m.FormatVer
	}

	var sb strings.Builder
	sb.WriteString(string(m.Filetype))
	sb.WriteByte('\n')
	sb.WriteString(ver)
	sb.WriteByte('\n')
	sb.WriteString("Version: ")
	sb.WriteString(m.Version)
	sb.WriteByte('\n')
	sb.WriteString("Instance: ")
	sb.WriteString(m.InstanceUUID.String())
	sb.WriteByte('\n')

	// VClock is ALWAYS emitted, even when empty ("VClock: {}"). Tarantool's
	// recovery validates the filename signature against this line (xlog.c
	// "signature check"), so omitting it makes the file unloadable — the
	// first xlog after bootstrap legitimately carries an empty vclock.
	sb.WriteString("VClock: ")
	sb.WriteString(m.VClock.String())
	sb.WriteByte('\n')

	// PrevVClock is omitted when empty: Tarantool writes it only for
	// non-initial xlogs, and snapshots never carry it.
	if len(m.PrevVClock) > 0 {
		sb.WriteString("PrevVClock: ")
		sb.WriteString(m.PrevVClock.String())
		sb.WriteByte('\n')
	}
	// Preserve any extras the caller chose to round-trip. We sort by key
	// for deterministic output.
	if len(m.Extras) > 0 {
		keys := make([]string, 0, len(m.Extras))
		for k := range m.Extras {
			keys = append(keys, k)
		}
		// Extras are almost always empty; use a tiny inline insertion sort
		// (sortStrings) rather than pulling "sort"/"slices" into this file
		// solely for this rare deterministic-ordering path.
		sortStrings(keys)

		for _, k := range keys {
			sb.WriteString(k)
			sb.WriteString(": ")
			sb.WriteString(m.Extras[k])
			sb.WriteByte('\n')
		}
	}

	sb.WriteByte('\n') // Blank-line terminator.

	if _, err := io.WriteString(w, sb.String()); err != nil {
		return fmt.Errorf("format: EncodeMeta: write: %w", err)
	}

	return nil
}

// DecodeMeta reads a meta header from r up to and including the blank-line
// terminator. The reader is left positioned at the first byte after the
// terminator (i.e. the magic byte of tx block 1 or the EOF marker).
//
// Strictness defaults: unrecognised filetypes / format versions return
// ErrMetaBadFormat / ErrMetaBadVersion.
func DecodeMeta(r *bufio.Reader, opts MetaOptions) (*Meta, error) {
	if r == nil {
		return nil, fmt.Errorf("format: DecodeMeta: %w", ErrNilMetaReader)
	}
	// Line 1: filetype.
	ftLine, err := readMetaLine(r)
	if err != nil {
		return nil, err
	}

	ft := Filetype(ftLine)
	switch ft {
	case FiletypeXLOG, FiletypeSNAP, FiletypeVYLOG, FiletypeRUN, FiletypeINDEX:
	default:
		return nil, fmt.Errorf("%w: unknown filetype %q", ErrMetaBadFormat, ftLine)
	}
	// Line 2: format version.
	verLine, err := readMetaLine(r)
	if err != nil {
		return nil, err
	}

	switch verLine {
	case FormatVersion:
		// Ok.
	case LegacyFormatVersion:
		if !opts.AcceptV012 {
			return nil, fmt.Errorf("%w: %q (need AcceptV012)", ErrMetaBadVersion, verLine)
		}
	default:
		return nil, fmt.Errorf("%w: %q", ErrMetaBadVersion, verLine)
	}

	m := &Meta{Filetype: ft, FormatVer: verLine}

	// Remaining lines until blank-line terminator: `Key: value` pairs.
	for {
		line, err := readMetaLine(r)
		if err != nil {
			return nil, err
		}

		if line == "" {
			// Blank line terminates the meta header.
			break
		}

		before, after, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%w: line %q has no colon", ErrMetaBadFormat, line)
		}

		key := strings.TrimSpace(before)
		val := strings.TrimSpace(after)

		switch key {
		case "Version":
			m.Version = val
		case "Instance", "Server": // "Server" is the legacy alias (pre-2.x).
			u, err := uuid.Parse(val)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid Instance UUID %q: %w", ErrMetaBadFormat, val, err)
			}

			m.InstanceUUID = u
		case "VClock":
			v, err := ParseVClock(val)
			if err != nil {
				return nil, fmt.Errorf("%w: VClock: %w", ErrMetaBadFormat, err)
			}

			m.VClock = v
		case "PrevVClock":
			v, err := ParseVClock(val)
			if err != nil {
				return nil, fmt.Errorf("%w: PrevVClock: %w", ErrMetaBadFormat, err)
			}

			m.PrevVClock = v
		default:
			if m.Extras == nil {
				m.Extras = map[string]string{}
			}

			m.Extras[key] = val
		}
	}

	return m, nil
}

// readMetaLine returns one '\n'-terminated line with the terminator stripped.
// Returns ErrMetaTruncated if the underlying reader EOFs mid-header.
func readMetaLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", ErrMetaTruncated
		}

		return "", fmt.Errorf("format: meta read: %w", err)
	}
	// Strip the trailing '\n' (and optional '\r' for tolerance).
	line = strings.TrimRight(line, "\n")
	line = strings.TrimRight(line, "\r")

	return line, nil
}

// sortStrings is a small insertion-sort helper that avoids pulling "sort" or
// "slices" into meta.go solely for the rare EncodeMeta extras-ordering path.
func sortStrings(a []string) {
	// Insertion sort — extras are typically 0 entries, occasionally a few.
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
