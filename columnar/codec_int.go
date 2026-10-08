package columnar

import (
	"encoding/binary"
	"fmt"
)

// This file implements the int64 codecs. Strings are dictionary-encoded
// to int64 ids before reaching these codecs (see codec_string.go), and
// floats are re-interpreted as uint64 bit patterns, so all on-disk value
// encodings operate on int64 streams.
//
// Payload formats (all integers little-endian):
//
//	PLAIN:   v0 v1 ... vN            (8 bytes each)
//	BITPACK: min(8) width(1) packed(v-min)
//	RLE:     [value(8) count(4)]*
//	DICT:    ndict(4) dict... (8 each) width(1) packed ids
//	DELTA:   first(8) delta(8)       (count comes from chunk metadata)

func encodeInts(id EncodingID, vals []int64) ([]byte, error) {
	switch id {
	case EncPlain:
		return encodePlain(vals), nil
	case EncBitPack:
		return encodeBitPack(vals), nil
	case EncRLE:
		return encodeRLE(vals), nil
	case EncDict:
		return encodeDict(vals), nil
	case EncDelta:
		return encodeDelta(vals), nil
	default:
		return nil, fmt.Errorf("%w: id %d", ErrUnknownEncoding, id)
	}
}

func decodeInts(id EncodingID, ver byte, data []byte, n int) ([]int64, error) {
	if ver == 0 || ver > encVersion {
		return nil, fmt.Errorf("%w: encoding %d version %d", ErrUnsupportedVersion, id, ver)
	}
	switch id {
	case EncPlain:
		return decodePlain(data, n)
	case EncBitPack:
		return decodeBitPack(data, n)
	case EncRLE:
		return decodeRLE(data, n)
	case EncDict:
		return decodeDict(data, n)
	case EncDelta:
		return decodeDelta(data, n)
	default:
		return nil, fmt.Errorf("%w: id %d", ErrUnknownEncoding, id)
	}
}

// chooseIntEncoding picks an encoding deterministically from the data.
func chooseIntEncoding(vals []int64) EncodingID {
	n := len(vals)
	if n == 0 {
		return EncPlain
	}
	// Constant run.
	if runsOf(vals) == 1 {
		return EncRLE
	}
	// Exact arithmetic progression.
	if n >= 3 && isArithmetic(vals) {
		return EncDelta
	}
	// Long runs.
	if runs := runsOf(vals); runs*2 <= n {
		return EncRLE
	}
	// Low cardinality.
	if d := distinctCount(vals); d <= 256 && d*2 <= n {
		return EncDict
	}
	// Narrow range.
	lo, hi := vals[0], vals[0]
	for _, v := range vals[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if bitWidth(uint64(hi)-uint64(lo)) <= 48 {
		return EncBitPack
	}
	return EncPlain
}

func runsOf(vals []int64) int {
	runs := 1
	for i := 1; i < len(vals); i++ {
		if vals[i] != vals[i-1] {
			runs++
		}
	}
	return runs
}

func isArithmetic(vals []int64) bool {
	d := vals[1] - vals[0]
	for i := 2; i < len(vals); i++ {
		if vals[i]-vals[i-1] != d {
			return false
		}
	}
	return true
}

func distinctCount(vals []int64) int {
	m := make(map[int64]struct{}, 256)
	for _, v := range vals {
		m[v] = struct{}{}
		if len(m) > 256 {
			return len(m)
		}
	}
	return len(m)
}

// --- PLAIN ---

func encodePlain(vals []int64) []byte {
	out := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint64(out[i*8:], uint64(v))
	}
	return out
}

func decodePlain(data []byte, n int) ([]int64, error) {
	if len(data) < n*8 {
		return nil, fmt.Errorf("%w: plain chunk needs %d bytes, have %d", ErrTruncated, n*8, len(data))
	}
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(binary.LittleEndian.Uint64(data[i*8:]))
	}
	return out, nil
}

// --- BITPACK ---

func encodeBitPack(vals []int64) []byte {
	if len(vals) == 0 {
		return make([]byte, 9)
	}
	lo := vals[0]
	for _, v := range vals[1:] {
		if v < lo {
			lo = v
		}
	}
	width := bitWidth(uint64(vals[0]) - uint64(lo))
	for _, v := range vals[1:] {
		if w := bitWidth(uint64(v) - uint64(lo)); w > width {
			width = w
		}
	}
	out := make([]byte, 0, 9+len(vals)*int(width)/8+1)
	var hdr [9]byte
	binary.LittleEndian.PutUint64(hdr[:8], uint64(lo))
	hdr[8] = byte(width)
	out = append(out, hdr[:]...)
	var w bitWriter
	w.buf = out
	for _, v := range vals {
		w.write(uint64(v)-uint64(lo), uint(width))
	}
	return w.bytes()
}

func decodeBitPack(data []byte, n int) ([]int64, error) {
	if len(data) < 9 {
		return nil, fmt.Errorf("%w: bitpack header", ErrTruncated)
	}
	lo := int64(binary.LittleEndian.Uint64(data[:8]))
	width := uint(data[8])
	if width > 64 {
		return nil, fmt.Errorf("%w: bitpack width %d", ErrCorrupt, width)
	}
	r := bitReader{buf: data[9:]}
	out := make([]int64, n)
	for i := range out {
		v, ok := r.read(width)
		if !ok {
			return nil, fmt.Errorf("%w: bitpack payload at value %d", ErrTruncated, i)
		}
		out[i] = lo + int64(v)
	}
	return out, nil
}

// --- RLE ---

func encodeRLE(vals []int64) []byte {
	out := make([]byte, 0, 24)
	var tmp [12]byte
	for i := 0; i < len(vals); {
		j := i + 1
		for j < len(vals) && vals[j] == vals[i] {
			j++
		}
		binary.LittleEndian.PutUint64(tmp[:8], uint64(vals[i]))
		binary.LittleEndian.PutUint32(tmp[8:], uint32(j-i))
		out = append(out, tmp[:]...)
		i = j
	}
	return out
}

func decodeRLE(data []byte, n int) ([]int64, error) {
	if len(data)%12 != 0 {
		return nil, fmt.Errorf("%w: rle length %d not a multiple of 12", ErrCorrupt, len(data))
	}
	out := make([]int64, 0, n)
	for off := 0; off < len(data); off += 12 {
		v := int64(binary.LittleEndian.Uint64(data[off:]))
		cnt := int(binary.LittleEndian.Uint32(data[off+8:]))
		if len(out)+cnt > n {
			return nil, fmt.Errorf("%w: rle expands beyond %d values", ErrCorrupt, n)
		}
		for k := 0; k < cnt; k++ {
			out = append(out, v)
		}
	}
	if len(out) != n {
		return nil, fmt.Errorf("%w: rle expanded to %d values, want %d", ErrCorrupt, len(out), n)
	}
	return out, nil
}

// --- DICT ---

func encodeDict(vals []int64) []byte {
	index := make(map[int64]uint64, 64)
	var dict []int64
	ids := make([]uint64, len(vals))
	for i, v := range vals {
		id, ok := index[v]
		if !ok {
			id = uint64(len(dict))
			index[v] = id
			dict = append(dict, v)
		}
		ids[i] = id
	}
	width := bitWidth(uint64(len(dict) - 1))
	out := make([]byte, 0, 4+8*len(dict)+1+len(vals))
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(dict)))
	out = append(out, hdr[:]...)
	var vb [8]byte
	for _, v := range dict {
		binary.LittleEndian.PutUint64(vb[:], uint64(v))
		out = append(out, vb[:]...)
	}
	out = append(out, byte(width))
	var w bitWriter
	w.buf = out
	for _, id := range ids {
		w.write(id, uint(width))
	}
	return w.bytes()
}

func decodeDict(data []byte, n int) ([]int64, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("%w: dict header", ErrTruncated)
	}
	nd := int(binary.LittleEndian.Uint32(data[:4]))
	if len(data) < 4+nd*8+1 {
		return nil, fmt.Errorf("%w: dict body needs %d entries", ErrTruncated, nd)
	}
	dict := make([]int64, nd)
	for i := range dict {
		dict[i] = int64(binary.LittleEndian.Uint64(data[4+i*8:]))
	}
	width := uint(data[4+nd*8])
	if width > 64 {
		return nil, fmt.Errorf("%w: dict width %d", ErrCorrupt, width)
	}
	r := bitReader{buf: data[5+nd*8:]}
	out := make([]int64, n)
	for i := range out {
		id, ok := r.read(width)
		if !ok {
			return nil, fmt.Errorf("%w: dict ids at value %d", ErrTruncated, i)
		}
		if id >= uint64(nd) {
			return nil, fmt.Errorf("%w: dict id %d out of range %d", ErrCorrupt, id, nd)
		}
		out[i] = dict[id]
	}
	return out, nil
}

// --- DELTA ---

func encodeDelta(vals []int64) []byte {
	out := make([]byte, 16)
	if len(vals) == 0 {
		return out
	}
	binary.LittleEndian.PutUint64(out[:8], uint64(vals[0]))
	if len(vals) > 1 {
		binary.LittleEndian.PutUint64(out[8:], uint64(vals[1]-vals[0]))
	}
	return out
}

func decodeDelta(data []byte, n int) ([]int64, error) {
	if len(data) < 16 {
		return nil, fmt.Errorf("%w: delta payload", ErrTruncated)
	}
	first := int64(binary.LittleEndian.Uint64(data[:8]))
	d := int64(binary.LittleEndian.Uint64(data[8:]))
	out := make([]int64, n)
	v := first
	for i := range out {
		out[i] = v
		v += d
	}
	return out, nil
}
