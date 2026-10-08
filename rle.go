package columnar

import "fmt"

// RLE encoding layout (version 1): a sequence of runs, each run is
//   plain-encoded value, then uvarint run length (>= 1).
// The sum of run lengths must equal the number of values exactly.

// valuesEqual reports lossless equality of two non-null same-kind values.
func valuesEqual(a, b Value) bool {
	if a.Kind != b.Kind || a.Null != b.Null {
		return false
	}
	switch a.Kind {
	case KindInt64:
		return a.I64 == b.I64
	case KindFloat64:
		// Bit-exact compare keeps -0/+0 and NaN payloads distinct.
		return canonicalF64(a.F64) == canonicalF64(b.F64)
	case KindString:
		return a.Str == b.Str
	case KindBool:
		return a.Bool == b.Bool
	}
	return false
}

func rleEncode(vals []Value) []byte {
	w := &byteWriter{}
	i := 0
	for i < len(vals) {
		j := i + 1
		for j < len(vals) && valuesEqual(vals[j], vals[i]) {
			j++
		}
		plainEncodeValue(w, vals[i])
		w.uvarint(uint64(j - i))
		i = j
	}
	return w.buf
}

func rleDecode(data []byte, n int, kind Kind) ([]Value, error) {
	r := &byteReader{buf: data}
	out := make([]Value, 0, n)
	for len(out) < n {
		v := plainDecodeValue(r, kind)
		if r.err != nil {
			return nil, r.err
		}
		cnt := r.uvarint()
		if r.err != nil {
			return nil, r.err
		}
		if cnt == 0 {
			return nil, fmt.Errorf("%w: rle run length 0", ErrCorrupt)
		}
		if uint64(n-len(out)) < cnt {
			return nil, fmt.Errorf("%w: rle runs overflow value count", ErrCorrupt)
		}
		for k := uint64(0); k < cnt; k++ {
			out = append(out, v)
		}
	}
	if err := r.finish(); err != nil {
		return nil, err
	}
	return out, nil
}
