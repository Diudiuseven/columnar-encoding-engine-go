package columnar

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

// Bit-pack encoding layout (version 1), integers only:
//   base:  8 bytes little-endian (the minimum value, as int64)
//   width: uvarint bit width of (value - base), 0..64
//   data:  ceil(n*width/8) bytes, deltas packed LSB-first
//
// Deltas are computed in uint64 arithmetic so the full int64 range
// (including MinInt64..MaxInt64 spreads) is lossless.

func bitpackEncode(vals []Value) ([]byte, error) {
	w := &byteWriter{}
	if len(vals) == 0 {
		var b [8]byte
		w.bytes(b[:])
		w.uvarint(0)
		return w.buf, nil
	}
	base := vals[0].I64
	for _, v := range vals {
		if v.Kind != KindInt64 {
			return nil, fmt.Errorf("%w: bitpack requires int64, got %s", ErrSchema, v.Kind)
		}
		if v.I64 < base {
			base = v.I64
		}
	}
	var maxDelta uint64
	for _, v := range vals {
		d := uint64(v.I64) - uint64(base)
		if d > maxDelta {
			maxDelta = d
		}
	}
	width := bits.Len64(maxDelta)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(base))
	w.bytes(b[:])
	w.uvarint(uint64(width))

	// Pack LSB-first.
	var acc uint64
	var nbits uint
	for _, v := range vals {
		d := uint64(v.I64) - uint64(base)
		acc |= d << nbits
		nbits += uint(width)
		for nbits >= 8 {
			w.u8(byte(acc))
			acc >>= 8
			nbits -= 8
		}
	}
	if nbits > 0 {
		w.u8(byte(acc))
	}
	return w.buf, nil
}

func bitpackDecode(data []byte, n int) ([]Value, error) {
	r := &byteReader{buf: data}
	b := r.bytes(8)
	if r.err != nil {
		return nil, r.err
	}
	base := int64(binary.LittleEndian.Uint64(b))
	width := r.uvarint()
	if r.err != nil {
		return nil, r.err
	}
	if width > 64 {
		return nil, fmt.Errorf("%w: bitpack width %d > 64", ErrCorrupt, width)
	}
	need := (uint64(n)*width + 7) / 8
	if uint64(r.remaining()) != need {
		return nil, fmt.Errorf("%w: bitpack payload size mismatch (need %d, have %d)",
			ErrCorrupt, need, r.remaining())
	}
	out := make([]Value, 0, n)
	var acc uint64
	var nbits uint
	for i := 0; i < n; i++ {
		for nbits < uint(width) {
			acc |= uint64(r.buf[r.off]) << nbits
			r.off++
			nbits += 8
		}
		var d uint64
		if width == 64 {
			d = acc
			acc = 0
		} else {
			d = acc & ((uint64(1) << width) - 1)
			acc >>= width
		}
		nbits -= uint(width)
		out = append(out, Int64(int64(uint64(base)+d)))
	}
	if err := r.finish(); err != nil {
		return nil, err
	}
	return out, nil
}
