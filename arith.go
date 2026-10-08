package columnar

import (
	"encoding/binary"
	"fmt"
)

// Arithmetic-sequence encoding layout (version 1), integers only:
//   start: 8 bytes little-endian int64
//   step:  8 bytes little-endian int64
// Value i is start + i*step. All arithmetic wraps mod 2^64; the encoder
// only selects this encoding when every value matches exactly, so it is
// lossless even across int64 overflow boundaries.

// arithFits reports whether vals form an exact arithmetic sequence and
// returns start and step.
func arithFits(vals []Value) (start, step int64, ok bool) {
	if len(vals) == 0 {
		return 0, 0, true
	}
	if vals[0].Kind != KindInt64 {
		return 0, 0, false
	}
	start = vals[0].I64
	if len(vals) == 1 {
		return start, 0, true
	}
	if vals[1].Kind != KindInt64 {
		return 0, 0, false
	}
	step = vals[1].I64 - start // wraps, verified below
	for i, v := range vals {
		if v.Kind != KindInt64 {
			return 0, 0, false
		}
		want := int64(uint64(start) + uint64(i)*uint64(step))
		if v.I64 != want {
			return 0, 0, false
		}
	}
	return start, step, true
}

func arithEncode(vals []Value) ([]byte, error) {
	start, step, ok := arithFits(vals)
	if !ok {
		return nil, fmt.Errorf("%w: values are not an arithmetic sequence", ErrSchema)
	}
	w := &byteWriter{}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(start))
	w.bytes(b[:])
	binary.LittleEndian.PutUint64(b[:], uint64(step))
	w.bytes(b[:])
	return w.buf, nil
}

func arithDecode(data []byte, n int) ([]Value, error) {
	r := &byteReader{buf: data}
	b := r.bytes(8)
	if r.err != nil {
		return nil, r.err
	}
	start := int64(binary.LittleEndian.Uint64(b))
	b = r.bytes(8)
	if r.err != nil {
		return nil, r.err
	}
	step := int64(binary.LittleEndian.Uint64(b))
	if err := r.finish(); err != nil {
		return nil, err
	}
	out := make([]Value, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Int64(int64(uint64(start)+uint64(i)*uint64(step))))
	}
	return out, nil
}
