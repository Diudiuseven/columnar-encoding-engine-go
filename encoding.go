package columnar

import "fmt"

// Encoding identifies the algorithm used for one column chunk.
type Encoding uint8

const (
	encInvalid Encoding = iota
	// EncPlain stores values one after another; the deterministic fallback.
	EncPlain
	// EncDict stores a dictionary plus bit-packed indices.
	EncDict
	// EncRLE stores (value, run-length) pairs.
	EncRLE
	// EncBitpack stores a base plus fixed-width deltas (integers only).
	EncBitpack
	// EncArith stores start/step for exact arithmetic sequences (integers).
	EncArith
)

// encVersion is the current per-encoding format version written by this
// library. Readers accept any version <= encVersion and reject higher
// versions with ErrUnsupportedVersion.
const encVersion uint8 = 1

func (e Encoding) String() string {
	switch e {
	case EncPlain:
		return "plain"
	case EncDict:
		return "dict"
	case EncRLE:
		return "rle"
	case EncBitpack:
		return "bitpack"
	case EncArith:
		return "arith"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(e))
	}
}

// encodeValues encodes non-null values with the given encoding.
func encodeValues(enc Encoding, vals []Value) ([]byte, error) {
	switch enc {
	case EncPlain:
		return plainEncode(vals), nil
	case EncDict:
		return dictEncode(vals)
	case EncRLE:
		return rleEncode(vals), nil
	case EncBitpack:
		return bitpackEncode(vals)
	case EncArith:
		return arithEncode(vals)
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedEncoding, uint8(enc))
	}
}

// decodeValues decodes exactly n non-null values of the given kind.
// data must be the exact payload; trailing or truncated bytes are errors.
func decodeValues(enc Encoding, ver uint8, data []byte, n int, kind Kind) ([]Value, error) {
	if ver == 0 || ver > encVersion {
		return nil, fmt.Errorf("%w: encoding %s version %d (supported <= %d)",
			ErrUnsupportedVersion, enc, ver, encVersion)
	}
	if n < 0 {
		return nil, fmt.Errorf("%w: negative value count %d", ErrCorrupt, n)
	}
	switch enc {
	case EncPlain:
		return plainDecode(data, n, kind)
	case EncDict:
		return dictDecode(data, n, kind)
	case EncRLE:
		return rleDecode(data, n, kind)
	case EncBitpack:
		return bitpackDecode(data, n)
	case EncArith:
		return arithDecode(data, n)
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedEncoding, uint8(enc))
	}
}

// byteReader is a bounds-checked cursor over a byte slice. Every failed
// read sets err; subsequent reads are no-ops. It never panics.
type byteReader struct {
	buf []byte
	off int
	err error
}

func (r *byteReader) remaining() int { return len(r.buf) - r.off }

func (r *byteReader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.remaining() < n {
		r.err = fmt.Errorf("%w: unexpected end of data (need %d, have %d)",
			ErrCorrupt, n, r.remaining())
		return nil
	}
	b := r.buf[r.off : r.off+n]
	r.off += n
	return b
}

func (r *byteReader) u8() uint8 {
	b := r.bytes(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *byteReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	var v uint64
	for i := 0; i < 10; i++ {
		b := r.u8()
		if r.err != nil {
			return 0
		}
		if b < 0x80 {
			if i == 9 && b > 1 {
				r.err = fmt.Errorf("%w: varint overflow", ErrCorrupt)
				return 0
			}
			return v | uint64(b)<<uint(7*i)
		}
		v |= uint64(b&0x7f) << uint(7*i)
	}
	r.err = fmt.Errorf("%w: varint too long", ErrCorrupt)
	return 0
}

// finish verifies the whole buffer was consumed.
func (r *byteReader) finish() error {
	if r.err != nil {
		return r.err
	}
	if r.remaining() != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrCorrupt, r.remaining())
	}
	return nil
}

// byteWriter appends deterministically.
type byteWriter struct{ buf []byte }

func (w *byteWriter) u8(v uint8)     { w.buf = append(w.buf, v) }
func (w *byteWriter) bytes(b []byte) { w.buf = append(w.buf, b...) }
func (w *byteWriter) uvarint(v uint64) {
	var tmp [10]byte
	n := 0
	for v >= 0x80 {
		tmp[n] = byte(v) | 0x80
		v >>= 7
		n++
	}
	tmp[n] = byte(v)
	w.buf = append(w.buf, tmp[:n+1]...)
}
