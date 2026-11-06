package format

// DecodeOptions configure the body decoders that fill a caller-owned struct
// (DecodeDMLBodyInto, DecodeRaftBodyInto, DecodeSynchroBodyInto,
// DecodeVyLogBodyInto, DecodeVyStmtBodyInto, DecodeVyRunInfoBodyInto and
// DecodeVyPageInfoBodyInto). A nil *DecodeOptions is the zero value: unknown
// keys are skipped.
type DecodeOptions struct {
	// OnUnknown, if non-nil, is called for each body key with no typed
	// field, in the order of the body. raw is the msgpack value of the key
	// and aliases the input; see Aliasing in the package doc. Nil means
	// unknown keys are skipped, and the decode stays allocation-free.
	//
	// A path that decodes a body and encodes it again must set OnUnknown and
	// write the keys it receives back, or the encoded body loses them.
	OnUnknown func(key uint64, raw []byte)
}

// CollectUnknown returns DecodeOptions whose OnUnknown stores every unknown
// body key and its raw msgpack value into *dst, making the map on the first
// one: *dst stays nil for a body with no unknown key. The values alias the
// input; see Aliasing in the package doc. A later key replaces an earlier
// duplicate. *dst is not cleared between decodes; set it to nil to start
// over.
func CollectUnknown(dst *map[uint64][]byte) *DecodeOptions {
	return &DecodeOptions{OnUnknown: func(key uint64, raw []byte) {
		if *dst == nil {
			*dst = map[uint64][]byte{}
		}

		(*dst)[key] = raw
	}}
}

// unknown hands an unknown body key to opts.OnUnknown, if any.
func (opts *DecodeOptions) unknown(key uint64, raw []byte) {
	if opts != nil && opts.OnUnknown != nil {
		opts.OnUnknown(key, raw)
	}
}
