package columnar

import (
	"fmt"
	"math/bits"
)

// Dictionary encoding layout (version 1):
//   uvarint dictionary length D
//   D plain-encoded values, in first-appearance order (deterministic)
//   uvarint index bit width W = bits needed for D-1 (0 when D <= 1)
//   ceil(n*W/8) bytes of indices packed LSB-first
//
// Works for every kind. Float keys use canonical bit patterns so NaN and
// -0/+0 stay distinct and lossless.

// dictKey maps a value to a comparable map key without losing information.
type dictKey struct {
	kind Kind
	i64  int64
	u64  uint64
	s    string
}

func makeDictKey(v Value) dictKey {
	k := dictKey{kind: v.Kind}
	switch v.Kind {
	case KindInt64:
		k.i64 = v.I64
	case KindFloat64:
		k.u64 = canonicalF64(v.F64)
	case KindString:
		k.s = v.Str
	case KindBool:
		if v.Bool {
			k.i64 = 1
		}
	}
	return k
}

func dictEncode(vals []Value) ([]byte, error) {
	w := &byteWriter{}
	dict := []Value{}
	index := map[dictKey]uint64{}
	indices := make([]uint64, len(vals))
	for i, v := range vals {
		key := makeDictKey(v)
		id, ok := index[key]
		if !ok {
			id = uint64(len(dict))
			index[key] = id
			dict = append(dict, v)
		}
		indices[i] = id
	}
	w.uvarint(uint64(len(dict)))
	for _, v := range dict {
		plainEncodeValue(w, v)
	}
	var width uint
	if len(dict) > 1 {
		width = uint(bits.Len64(uint64(len(dict) - 1)))
	}
	w.uvarint(uint64(width))
	w.bytes(packIndices(indices, width))
	return w.buf, nil
}

func dictDecode(data []byte, n int, kind Kind) ([]Value, error) {
	r := &byteReader{buf: data}
	d := r.uvarint()
	if r.err != nil {
		return nil, r.err
	}
	if d > uint64(r.remaining()) && kind != KindString {
		// Each non-string entry needs at least 1 byte; cheap sanity cap
		// to avoid huge allocations on corrupt input.
		return nil, fmt.Errorf("%w: dictionary length %d implausible", ErrCorrupt, d)
	}
	dict := make([]Value, 0, d)
	for i := uint64(0); i < d; i++ {
		v := plainDecodeValue(r, kind)
		if r.err != nil {
			return nil, r.err
		}
		dict = append(dict, v)
	}
	width := uint(r.uvarint())
	if r.err != nil {
		return nil, r.err
	}
	if width > 64 {
		return nil, fmt.Errorf("%w: dict index width %d > 64", ErrCorrupt, width)
	}
	indices, err := unpackIndices(r, n, width)
	if err != nil {
		return nil, err
	}
	if err := r.finish(); err != nil {
		return nil, err
	}
	out := make([]Value, 0, n)
	for _, id := range indices {
		if id >= uint64(len(dict)) {
			return nil, fmt.Errorf("%w: dict index %d out of range %d", ErrCorrupt, id, len(dict))
		}
		out = append(out, dict[id])
	}
	return out, nil
}

// packIndices packs unsigned indices LSB-first with the given width.
func packIndices(indices []uint64, width uint) []byte {
	if width == 0 {
		return nil
	}
	w := &byteWriter{}
	var acc uint64
	var nbits uint
	for _, id := range indices {
		acc |= id << nbits
		nbits += width
		for nbits >= 8 {
			w.u8(byte(acc))
			acc >>= 8
			nbits -= 8
		}
	}
	if nbits > 0 {
		w.u8(byte(acc))
	}
	return w.buf
}

// unpackIndices reads exactly n indices of the given width from r.
func unpackIndices(r *byteReader, n int, width uint) ([]uint64, error) {
	if width == 0 {
		return make([]uint64, n), nil
	}
	need := (uint64(n)*uint64(width) + 7) / 8
	if uint64(r.remaining()) < need {
		return nil, fmt.Errorf("%w: not enough packed index bytes (need %d, have %d)",
			ErrCorrupt, need, r.remaining())
	}
	out := make([]uint64, 0, n)
	var acc uint64
	var nbits uint
	for i := 0; i < n; i++ {
		for nbits < width {
			acc |= uint64(r.buf[r.off]) << nbits
			r.off++
			nbits += 8
		}
		var id uint64
		if width == 64 {
			id = acc
			acc = 0
		} else {
			id = acc & ((uint64(1) << width) - 1)
			acc >>= width
		}
		nbits -= width
		out = append(out, id)
	}
	return out, nil
}
