package format

import "bytes"

// This file copies decoded values out of the buffer they alias. The contract
// of Clone and CloneInto - what dst must own, nil and empty, maps - is in
// Aliasing in the package doc.

// Clone returns a deep copy of r: its BodyRaw is a new slice. A nil receiver
// returns nil.
//
// Cloning a row does not detach a body decoded from r.BodyRaw before: that
// body aliases the buffer r.BodyRaw did. Decode the body from the BodyRaw of
// the clone, or clone the body too.
func (r *XRow) Clone() *XRow {
	if r == nil {
		return nil
	}

	out := *r
	out.BodyRaw = bytes.Clone(r.BodyRaw)

	return &out
}

// CloneInto deep-copies r into dst, overwriting every field, and copies
// r.BodyRaw into the array of dst.BodyRaw when it has the capacity. dst must
// own its slices; see Aliasing in the package doc. dst may be r.
func (r *XRow) CloneInto(dst *XRow) {
	body := cloneBytesInto(dst.BodyRaw, r.BodyRaw)
	*dst = *r
	dst.BodyRaw = body
}

// Clone returns a deep copy of b: its Tuple, Key, Ops, OldTuple, NewTuple and
// Arrow are copied into one new allocation. A nil receiver returns nil.
func (b *DMLBody) Clone() *DMLBody {
	if b == nil {
		return nil
	}

	out := *b
	arena := newCloneArena(len(b.Tuple) + len(b.Key) + len(b.Ops) + len(b.OldTuple) + len(b.NewTuple) + len(b.Arrow))
	out.Tuple = arena.clone(b.Tuple)
	out.Key = arena.clone(b.Key)
	out.Ops = arena.clone(b.Ops)
	out.OldTuple = arena.clone(b.OldTuple)
	out.NewTuple = arena.clone(b.NewTuple)
	out.Arrow = arena.clone(b.Arrow)

	return &out
}

// CloneInto deep-copies b into dst, overwriting every field, and copies each
// raw field of b into the array of the same field of dst when it has the
// capacity. dst must own its slices; see Aliasing in the package doc. dst
// may be b.
func (b *DMLBody) CloneInto(dst *DMLBody) {
	tuple := cloneBytesInto(dst.Tuple, b.Tuple)
	key := cloneBytesInto(dst.Key, b.Key)
	ops := cloneBytesInto(dst.Ops, b.Ops)
	oldTuple := cloneBytesInto(dst.OldTuple, b.OldTuple)
	newTuple := cloneBytesInto(dst.NewTuple, b.NewTuple)
	arrow := cloneBytesInto(dst.Arrow, b.Arrow)

	*dst = *b
	dst.Tuple, dst.Key, dst.Ops = tuple, key, ops
	dst.OldTuple, dst.NewTuple, dst.Arrow = oldTuple, newTuple, arrow
}

// Clone returns a deep copy of b. A RaftBody aliases nothing, but its VClock
// is a map, which the copy gets its own of. A nil receiver returns nil.
func (b *RaftBody) Clone() *RaftBody {
	if b == nil {
		return nil
	}

	out := *b
	out.VClock = b.VClock.Clone()

	return &out
}

// CloneInto deep-copies b into dst, overwriting every field. The VClock of
// dst is a new map, or nil when that of b is. dst may be b.
func (b *RaftBody) CloneInto(dst *RaftBody) {
	vc := b.VClock.Clone()
	*dst = *b
	dst.VClock = vc
}

// Clone returns a deep copy of b. A SynchroBody aliases nothing, but its
// ConfirmedVClock is a map, which the copy gets its own of. A nil receiver
// returns nil.
func (b *SynchroBody) Clone() *SynchroBody {
	if b == nil {
		return nil
	}

	out := *b
	out.ConfirmedVClock = b.ConfirmedVClock.Clone()

	return &out
}

// CloneInto deep-copies b into dst, overwriting every field. The
// ConfirmedVClock of dst is a new map, or nil when that of b is. dst may be b.
func (b *SynchroBody) CloneInto(dst *SynchroBody) {
	vc := b.ConfirmedVClock.Clone()
	*dst = *b
	dst.ConfirmedVClock = vc
}

// Clone returns a deep copy of b: a new Keys map whose values are copied into
// one new allocation. A nil receiver returns nil.
func (b *VyLogBody) Clone() *VyLogBody {
	if b == nil {
		return nil
	}

	return &VyLogBody{Type: b.Type, Keys: cloneVyKeys(b.Keys)}
}

// CloneInto deep-copies b into dst, overwriting every field. Keys is a map,
// which is never reused: dst gets a new one, as from Clone. dst may be b.
func (b *VyLogBody) CloneInto(dst *VyLogBody) {
	keys := cloneVyKeys(b.Keys)
	*dst = *b
	dst.Keys = keys
}

// cloneVyKeys deep-copies the fields of a vinyl log record.
func cloneVyKeys(keys map[VyKey][]byte) map[VyKey][]byte {
	if keys == nil {
		return nil
	}

	n := 0
	for _, v := range keys {
		n += len(v)
	}

	arena := newCloneArena(n)
	out := make(map[VyKey][]byte, len(keys))

	for k, v := range keys {
		out[k] = arena.clone(v)
	}

	return out
}

// Clone returns a deep copy of b: its Tuple, Key and Ops are copied into one
// new allocation. A nil receiver returns nil.
func (b *VyStmtBody) Clone() *VyStmtBody {
	if b == nil {
		return nil
	}

	out := *b
	arena := newCloneArena(len(b.Tuple) + len(b.Key) + len(b.Ops))
	out.Tuple = arena.clone(b.Tuple)
	out.Key = arena.clone(b.Key)
	out.Ops = arena.clone(b.Ops)

	return &out
}

// CloneInto deep-copies b into dst, overwriting every field, and copies each
// raw field of b into the array of the same field of dst when it has the
// capacity. dst must own its slices; see Aliasing in the package doc. dst
// may be b.
func (b *VyStmtBody) CloneInto(dst *VyStmtBody) {
	tuple := cloneBytesInto(dst.Tuple, b.Tuple)
	key := cloneBytesInto(dst.Key, b.Key)
	ops := cloneBytesInto(dst.Ops, b.Ops)

	*dst = *b
	dst.Tuple, dst.Key, dst.Ops = tuple, key, ops
}

// Clone returns a deep copy of info: its MinKey, MaxKey and Bloom are copied
// into one new allocation. A nil receiver returns nil.
func (info *VyRunInfo) Clone() *VyRunInfo {
	if info == nil {
		return nil
	}

	out := *info
	arena := newCloneArena(len(info.MinKey) + len(info.MaxKey) + len(info.Bloom))
	out.MinKey = arena.clone(info.MinKey)
	out.MaxKey = arena.clone(info.MaxKey)
	out.Bloom = arena.clone(info.Bloom)

	return &out
}

// CloneInto deep-copies info into dst, overwriting every field, and copies
// each raw field of info into the array of the same field of dst when it has
// the capacity. dst must own its slices; see Aliasing in the package doc.
// dst may be info.
func (info *VyRunInfo) CloneInto(dst *VyRunInfo) {
	minKey := cloneBytesInto(dst.MinKey, info.MinKey)
	maxKey := cloneBytesInto(dst.MaxKey, info.MaxKey)
	bloom := cloneBytesInto(dst.Bloom, info.Bloom)

	*dst = *info
	dst.MinKey, dst.MaxKey, dst.Bloom = minKey, maxKey, bloom
}

// Clone returns a deep copy of page: its MinKey is a new slice. A nil
// receiver returns nil.
func (page *VyPageInfo) Clone() *VyPageInfo {
	if page == nil {
		return nil
	}

	out := *page
	out.MinKey = bytes.Clone(page.MinKey)

	return &out
}

// CloneInto deep-copies page into dst, overwriting every field, and copies
// page.MinKey into the array of dst.MinKey when it has the capacity. dst
// must own its slices; see Aliasing in the package doc. dst may be page.
func (page *VyPageInfo) CloneInto(dst *VyPageInfo) {
	minKey := cloneBytesInto(dst.MinKey, page.MinKey)
	*dst = *page
	dst.MinKey = minKey
}

// Clone returns a deep copy of op: its FieldName and Value are copied into
// one new allocation. It returns a value, as DecodeOps does.
func (op *Operation) Clone() Operation {
	out := *op
	arena := newCloneArena(len(op.FieldName) + len(op.Value))
	out.FieldName = arena.clone(op.FieldName)
	out.Value = arena.clone(op.Value)

	return out
}

// CloneInto deep-copies op into dst, overwriting every field, and copies
// FieldName and Value into the arrays of those of dst when they have the
// capacity. dst must own its slices; see Aliasing in the package doc. dst
// may be op.
func (op *Operation) CloneInto(dst *Operation) {
	name := cloneBytesInto(dst.FieldName, op.FieldName)
	value := cloneBytesInto(dst.Value, op.Value)

	*dst = *op
	dst.FieldName, dst.Value = name, value
}

// CloneOps returns a deep copy of ops: a new slice whose operations share no
// memory with ops, their FieldName and Value copied into one allocation. nil
// returns nil, and an empty non-nil ops an empty non-nil slice.
func CloneOps(ops []Operation) []Operation {
	if ops == nil {
		return nil
	}

	n := 0
	for i := range ops {
		n += len(ops[i].FieldName) + len(ops[i].Value)
	}

	arena := newCloneArena(n)
	out := make([]Operation, len(ops))

	for i := range ops {
		out[i] = ops[i]
		out[i].FieldName = arena.clone(ops[i].FieldName)
		out[i].Value = arena.clone(ops[i].Value)
	}

	return out
}

// CloneOpsInto is CloneOps into dst, as DecodeOpsInto is DecodeOps: it
// deep-copies the operations of src into dst[:len(src)], reusing the capacity
// of dst and, by Operation.CloneInto, that of the operations it holds up to
// its capacity, and returns the result. It does not allocate when dst holds
// as many operations as src, each with the capacity for its bytes. The
// operations of dst must own their slices, as for CloneInto. A nil src
// returns nil.
func CloneOpsInto(src, dst []Operation) []Operation {
	if src == nil {
		return nil
	}

	if cap(dst) < len(src) {
		// Keep the operations dst holds up to its capacity, and their bytes.
		dst = append(dst[:cap(dst)], make([]Operation, len(src)-cap(dst))...)
	}

	if dst == nil {
		// src is empty: keep it non-nil.
		return []Operation{}
	}

	dst = dst[:len(src)]
	for i := range src {
		src[i].CloneInto(&dst[i])
	}

	return dst
}

// cloneBytesInto copies src into the array of dst when it has the capacity
// and is not that of src, and into a new slice otherwise. nil stays nil and
// an empty non-nil src gives an empty non-nil slice.
func cloneBytesInto(dst, src []byte) []byte {
	switch {
	case src == nil:
		return nil
	case cap(dst) < len(src) || sameArray(dst, src):
		return bytes.Clone(src)
	case dst == nil:
		// src is empty, as cap(dst) >= len(src): keep it non-nil.
		return []byte{}
	default:
		dst = dst[:len(src)]
		copy(dst, src)

		return dst
	}
}

// sameArray reports whether a and b are slices of the same array: whether
// the last element of their capacity is the same. Two sub-slices of one
// buffer made without a capacity limit, as the decoders make them, end their
// capacity at the end of the buffer.
func sameArray(a, b []byte) bool {
	return cap(a) > 0 && cap(b) > 0 && &a[:cap(a)][cap(a)-1] == &b[:cap(b)][cap(b)-1]
}

// cloneArena copies byte slices into one allocation, so the Clone of a value
// with several raw fields allocates their bytes once. Every slice it returns
// has a capacity of its length: a CloneInto that later reuses one cannot grow
// into the next.
type cloneArena []byte

// newCloneArena returns an arena of n bytes, allocating nothing for n == 0.
func newCloneArena(n int) cloneArena {
	if n == 0 {
		return nil
	}

	return make(cloneArena, 0, n)
}

// clone copies src into the arena. nil stays nil and an empty non-nil src
// gives an empty non-nil slice.
func (a *cloneArena) clone(src []byte) []byte {
	switch {
	case src == nil:
		return nil
	case len(src) == 0:
		return []byte{}
	}

	start := len(*a)
	*a = append(*a, src...)
	end := len(*a)

	return (*a)[start:end:end]
}
