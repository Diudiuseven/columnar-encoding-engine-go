package columnar

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Plain encoding layout (version 1), values concatenated with no padding:
//   int64:   8 bytes little-endian
//   float64: 8 bytes little-endian IEEE-754 bits
//   bool:    1 byte (0 or 1)
//   string:  uvarint byte length, then raw bytes
//
// Float bits are canonicalized so that NaN always encodes to one fixed
// pattern, keeping output byte-reproducible.

// canonicalF64 maps every NaN to a single quiet-NaN bit pattern and -0 to
// itself (kept distinct from +0 to stay lossless).
func canonicalF64(f float64) uint64 {
	if math.IsNaN(f) {
		return 0x7ff8000000000001
	}
	return math.Float64bits(f)
}

func plainEncodeValue(w *byteWriter, v Value) {
	switch v.Kind {
	case KindInt64:
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(v.I64))
		w.bytes(b[:])
	case KindFloat64:
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], canonicalF64(v.F64))
		w.bytes(b[:])
	case KindBool:
		if v.Bool {
			w.u8(1)
		} else {
			w.u8(0)
		}
	case KindString:
		w.uvarint(uint64(len(v.Str)))
		w.bytes([]byte(v.Str))
	}
}

func plainEncode(vals []Value) []byte {
	w := &byteWriter{}
	for _, v := range vals {
		plainEncodeValue(w, v)
	}
	return w.buf
}

func plainDecodeValue(r *byteReader, kind Kind) Value {
	switch kind {
	case KindInt64:
		b := r.bytes(8)
		if b == nil {
			return Value{}
		}
		return Int64(int64(binary.LittleEndian.Uint64(b)))
	case KindFloat64:
		b := r.bytes(8)
		if b == nil {
			return Value{}
		}
		return Float64(math.Float64frombits(binary.LittleEndian.Uint64(b)))
	case KindBool:
		b := r.u8()
		if r.err != nil {
			return Value{}
		}
		if b > 1 {
			r.err = fmt.Errorf("%w: invalid bool byte %d", ErrCorrupt, b)
			return Value{}
		}
		return Bool(b == 1)
	case KindString:
		n := r.uvarint()
		if r.err != nil {
			return Value{}
		}
		if uint64(n) > uint64(r.remaining()) {
			r.err = fmt.Errorf("%w: string length %d exceeds remaining %d",
				ErrCorrupt, n, r.remaining())
			return Value{}
		}
		b := r.bytes(int(n))
		if b == nil {
			return Value{}
		}
		return String(string(b))
	}
	r.err = fmt.Errorf("%w: invalid kind %d", ErrCorrupt, uint8(kind))
	return Value{}
}

func plainDecode(data []byte, n int, kind Kind) ([]Value, error) {
	r := &byteReader{buf: data}
	out := make([]Value, 0, n)
	for i := 0; i < n; i++ {
		v := plainDecodeValue(r, kind)
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, v)
	}
	if err := r.finish(); err != nil {
		return nil, err
	}
	return out, nil
}
