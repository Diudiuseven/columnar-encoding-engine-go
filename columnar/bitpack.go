package columnar

import "math/bits"

// bitWriter appends values LSB-first into a byte slice.
type bitWriter struct {
	buf []byte
	acc uint64
	n   uint // bits held in acc
}

func (w *bitWriter) write(v uint64, width uint) {
	if width == 0 {
		return
	}
	w.acc |= v << w.n
	w.n += width
	for w.n >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.n -= 8
	}
}

func (w *bitWriter) bytes() []byte {
	if w.n > 0 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc, w.n = 0, 0
	}
	return w.buf
}

// bitReader reads values LSB-first. Reads beyond the end return ok=false
// so callers can report truncation instead of panicking.
type bitReader struct {
	buf []byte
	pos int // bit position
}

func (r *bitReader) read(width uint) (uint64, bool) {
	if width == 0 {
		return 0, true
	}
	if r.pos+int(width) > len(r.buf)*8 {
		return 0, false
	}
	var v uint64
	for i := uint(0); i < width; i++ {
		bit := (r.buf[r.pos>>3] >> (uint(r.pos) & 7)) & 1
		v |= uint64(bit) << i
		r.pos++
	}
	return v, true
}

// bitWidth returns the number of bits needed to represent v.
func bitWidth(v uint64) uint {
	return uint(bits.Len64(v))
}
