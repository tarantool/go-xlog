package compat //nolint:testpackage // white-box: defines renderFile/typeName consumed by l1_golden_test.go and l2_manifest_test.go, and consumes corpusFixtures from harness_test.go

import (
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// renderFile decodes the journal/storage file at path with the current library
// and returns a deterministic, line-oriented text rendering: one `meta` line
// followed by one `row` line per decoded row.
//
// A row-level decode failure is embedded inline as a `!decode-error` / a
// `!row-error` marker rather than crashing — the marker makes a reader gap
// visible in the golden. An Open/meta failure is a hard error.
func renderFile(path string) (string, error) {
	r, err := reader.Open(path)
	if err != nil {
		return "", fmt.Errorf("compat: open %q: %w", path, err)
	}

	defer func() { _ = r.Close() }()

	var b strings.Builder
	b.WriteString(renderMeta(r.Meta()))
	b.WriteByte('\n')

	ft := r.Meta().Filetype
	for row, err := range r.Rows() {
		if err != nil {
			fmt.Fprintf(&b, "!row-error: %v\n", err)

			break
		}

		b.WriteString(renderRow(ft, row))
		b.WriteByte('\n')
	}

	return b.String(), nil
}

func renderMeta(m *format.Meta) string {
	return fmt.Sprintf("meta filetype=%s format=%s version=%q instance=%s vclock=%s prev_vclock=%s extras=%s",
		m.Filetype, m.FormatVer, m.Version, m.InstanceUUID,
		renderVClock(m.VClock), renderVClock(m.PrevVClock), renderStrMap(m.Extras))
}

func renderRow(ft format.Filetype, r format.XRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "row lsn=%d replica=%d", r.LSN, r.ReplicaID)

	switch ft {
	case format.FiletypeXLOG, format.FiletypeSNAP:
		fmt.Fprintf(&b, " type=%s tsn=%d commit=%t flags=0x%02x group=%d",
			typeName(r.Type), r.TSN, r.IsCommit(), uint8(r.Flags), r.GroupID)

		if r.Timestamp != 0 {
			fmt.Fprintf(&b, " ts=%s", strconv.FormatFloat(r.Timestamp, 'g', -1, 64))
		}

		b.WriteString(" " + renderJournalBody(r.Type, r.BodyRaw))
	case format.FiletypeVYLOG, format.FiletypeRUN, format.FiletypeINDEX:
		// Vinyl artefacts, no iproto row semantics.
		fmt.Fprintf(&b, " type=%d %s", r.Type, renderStorageBody(ft, r.BodyRaw))
	default:
		fmt.Fprintf(&b, " type=%d %s", r.Type, renderStorageBody(ft, r.BodyRaw))
	}

	return b.String()
}

// renderJournalBody dispatches an xlog/snap row body to the typed decoder
// selected by the iproto request type.
func renderJournalBody(typ iproto.Type, body []byte) string {
	switch typ {
	case iproto.IPROTO_INSERT, iproto.IPROTO_REPLACE, iproto.IPROTO_UPDATE, iproto.IPROTO_DELETE, iproto.IPROTO_UPSERT:
		d, err := format.DecodeDMLBody(body)
		if err != nil {
			return "!decode-error: dml: " + err.Error()
		}

		out := fmt.Sprintf("dml space=%d", d.SpaceID)
		out += rawField(" tuple=", d.Tuple)
		out += rawField(" key=", d.Key)
		out += rawField(" ops=", d.Ops)
		out += rawField(" old=", d.OldTuple)

		out += rawField(" new=", d.NewTuple)
		if len(d.Extras) > 0 {
			out += " extras=" + renderU64Map(d.Extras)
		}

		return out
	case iproto.IPROTO_NOP:
		return "nop"
	case iproto.IPROTO_RAFT:
		d, err := format.DecodeRaftBody(body)
		if err != nil {
			return "!decode-error: raft: " + err.Error()
		}

		return fmt.Sprintf("raft term=%d vote=%d state=%d leader=%d leader_seen=%t vclock=%s",
			d.Term, d.Vote, d.State, d.LeaderID, d.IsLeaderSeen, renderVClock(d.VClock))
	case iproto.IPROTO_RAFT_CONFIRM, iproto.IPROTO_RAFT_ROLLBACK, iproto.IPROTO_RAFT_PROMOTE, iproto.IPROTO_RAFT_DEMOTE:
		// PROMOTE/DEMOTE (31/32) share the synchro body schema with
		// CONFIRM/ROLLBACK (40/41) — replica_id/lsn/term — not the raft
		// state body (Tarantool xrow_decode_synchro).
		d, err := format.DecodeSynchroBody(body)
		if err != nil {
			return "!decode-error: synchro: " + err.Error()
		}

		return fmt.Sprintf("synchro replica=%d lsn=%d term=%d", d.ReplicaID, d.LSN, d.Term)
	default:
		if len(body) == 0 {
			return "body=(none)"
		}

		return "body=0x" + hex.EncodeToString(body)
	}
}

// renderStorageBody renders a vinyl-artefact row body. VYLOG rows have a typed
// record decoder; RUN/INDEX bodies are rendered as canonical msgpack.
func renderStorageBody(ft format.Filetype, body []byte) string {
	if ft == format.FiletypeVYLOG {
		d, err := format.DecodeVyLogBody(body)
		if err != nil {
			return "!decode-error: vylog: " + err.Error()
		}

		keys := make(map[uint64][]byte, len(d.Keys))
		for k, v := range d.Keys {
			keys[uint64(k)] = v
		}

		return fmt.Sprintf("vylog rectype=%d keys=%s", d.Type, renderU64Map(keys))
	}

	if len(body) == 0 {
		return "body=(none)"
	}

	return "body=" + renderMP(body)
}

func rawField(label string, raw []byte) string {
	if len(raw) == 0 {
		return ""
	}

	return label + renderMP(raw)
}

// renderMP decodes a single msgpack value and renders it deterministically
// (sorted map keys, hex for binary). Falls back to a hex dump if the bytes are
// not a single well-formed msgpack value.
func renderMP(b []byte) string {
	var v any

	err := msgpack.Unmarshal(b, &v)
	if err != nil {
		return "0x" + hex.EncodeToString(b)
	}

	return renderValue(v)
}

func renderValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "nil"
	case bool:
		return strconv.FormatBool(x)
	case string:
		return strconv.Quote(x)
	case []byte:
		return "0x" + hex.EncodeToString(x)
	case int8, int16, int32, int64, int:
		return fmt.Sprintf("%d", x)
	case uint8, uint16, uint32, uint64, uint:
		return fmt.Sprintf("%d", x)
	case float32, float64:
		return strconv.FormatFloat(toFloat(x), 'g', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = renderValue(e)
		}

		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = strconv.Quote(k) + ":" + renderValue(x[k])
		}

		return "{" + strings.Join(parts, ",") + "}"
	case map[any]any:
		return renderIfaceMap(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// renderIfaceMap renders a map with arbitrary keys deterministically by sorting
// on the rendered key string.
func renderIfaceMap(m map[any]any) string {
	type kv struct{ k, v string }

	kvs := make([]kv, 0, len(m))
	for k, v := range m {
		kvs = append(kvs, kv{renderValue(k), renderValue(v)})
	}

	sort.Slice(kvs, func(i, j int) bool { return kvs[i].k < kvs[j].k })

	parts := make([]string, len(kvs))
	for i, e := range kvs {
		parts[i] = e.k + ":" + e.v
	}

	return "{" + strings.Join(parts, ",") + "}"
}

func toFloat(x any) float64 {
	switch f := x.(type) {
	case float32:
		return float64(f)
	case float64:
		return f
	}

	return 0
}

// renderVClock renders a VClock as {replica:lsn,...} sorted by replica id.
func renderVClock(v format.VClock) string {
	keys := make([]int, 0, len(v))
	for k := range v {
		keys = append(keys, int(k))
	}

	sort.Ints(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d:%d", k, v[uint32(k)])
	}

	return "{" + strings.Join(parts, ",") + "}"
}

func renderU64Map(m map[uint64][]byte) string {
	keys := make([]uint64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d:%s", k, renderMP(m[k]))
	}

	return "{" + strings.Join(parts, ",") + "}"
}

func renderStrMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strconv.Quote(k) + ":" + strconv.Quote(m[k])
	}

	return "{" + strings.Join(parts, ",") + "}"
}

// typeName maps an iproto request type to its name. Used only for XLOG/SNAP
// rows; vinyl artefacts render the numeric type instead.
func typeName(t iproto.Type) string {
	switch t {
	case iproto.IPROTO_INSERT:
		return "INSERT"
	case iproto.IPROTO_REPLACE:
		return "REPLACE"
	case iproto.IPROTO_UPDATE:
		return "UPDATE"
	case iproto.IPROTO_DELETE:
		return "DELETE"
	case iproto.IPROTO_UPSERT:
		return "UPSERT"
	case iproto.IPROTO_NOP:
		return "NOP"
	case iproto.IPROTO_RAFT:
		return "RAFT"
	case iproto.IPROTO_RAFT_PROMOTE:
		return "RAFT_PROMOTE"
	case iproto.IPROTO_RAFT_DEMOTE:
		return "RAFT_DEMOTE"
	case iproto.IPROTO_RAFT_CONFIRM:
		return "CONFIRM"
	case iproto.IPROTO_RAFT_ROLLBACK:
		return "ROLLBACK"
	default:
		return "TYPE(" + strconv.FormatUint(uint64(t), 10) + ")"
	}
}

// TestRender_Deterministic is the focused unit test for round-trip fidelity: the renderer must
// produce byte-identical output across runs (including stable map-key order).
func TestRender_Deterministic(t *testing.T) {
	t.Parallel()

	// Map-keyed helpers must emit sorted keys regardless of Go map order.
	u64 := map[uint64][]byte{
		3: mustMP(t, "c"), 1: mustMP(t, "a"), 2: mustMP(t, "b"),
	}
	assert.Equal(t, `{1:"a",2:"b",3:"c"}`, renderU64Map(u64), "renderU64Map not sorted")

	vc := format.VClock{2: 20, 1: 10, 5: 50}
	assert.Equal(t, "{1:10,2:20,5:50}", renderVClock(vc), "renderVClock not sorted")

	// Full-file render is stable across repeated calls over a frozen fixture.
	fixtures := corpusFixtures(t)
	require.NotEmpty(t, fixtures, "no corpus fixtures discovered")

	for _, f := range fixtures {
		a, err := renderFile(f)
		require.NoErrorf(t, err, "render %s", f)
		b, err := renderFile(f)
		require.NoErrorf(t, err, "render(2) %s", f)
		assert.Equalf(t, a, b, "non-deterministic render for %s", f)
	}
}

func mustMP(t *testing.T, s string) []byte {
	t.Helper()

	b, err := msgpack.Marshal(s)
	require.NoError(t, err)

	return b
}
