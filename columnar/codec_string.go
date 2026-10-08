package columnar

import (
	"encoding/binary"
	"fmt"
)

// String chunk payload formats:
//
//	PLAIN: [len(4) bytes]*
//	DICT:  ndict(4) [len(4) bytes]* width(1) packed ids
//
// Dictionary entries are stored in first-appearance order, which is
// deterministic for a given input.

func encodeStrings(id EncodingID, vals []string) ([]byte, error) {
	switch id {
	case EncPlain:
		return encodePlainStrings(vals), nil
	case EncDict:
		return encodeDictStrings(vals), nil
	default:
		return nil, fmt.Errorf("%w: string encoding id %d", ErrUnknownEncoding, id)
	}
}

func decodeStrings(id EncodingID, ver byte, data []byte, n int) ([]string, error) {
	if ver == 0 || ver > encVersion {
		return nil, fmt.Errorf("%w: encoding %d version %d", ErrUnsupportedVersion, id, ver)
	}
	switch id {
	case EncPlain:
		return decodePlainStrings(data, n)
	case EncDict:
		return decodeDictStrings(data, n)
	default:
		return nil, fmt.Errorf("%w: string encoding id %d", ErrUnknownEncoding, id)
	}
}

// chooseStringEncoding picks deterministically: dictionary when
// cardinality is low relative to the row count, plain otherwise.
func chooseStringEncoding(vals []string) EncodingID {
	n := len(vals)
	if n == 0 {
		return EncPlain
	}
	m := make(map[string]struct{}, 256)
	for _, v := range vals {
		m[v] = struct{}{}
		if len(m) > 4096 {
			return EncPlain
		}
	}
	if len(m)*2 <= n {
		return EncDict
	}
	return EncPlain
}

func encodePlainStrings(vals []string) []byte {
	total := 0
	for _, v := range vals {
		total += 4 + len(v)
	}
	out := make([]byte, 0, total)
	var lb [4]byte
	for _, v := range vals {
		binary.LittleEndian.PutUint32(lb[:], uint32(len(v)))
		out = append(out, lb[:]...)
		out = append(out, v...)
	}
	return out
}

func decodePlainStrings(data []byte, n int) ([]string, error) {
	out := make([]string, 0, n)
	off := 0
	for i := 0; i < n; i++ {
		if len(data)-off < 4 {
			return nil, fmt.Errorf("%w: string length at value %d", ErrTruncated, i)
		}
		l := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		if len(data)-off < l {
			return nil, fmt.Errorf("%w: string body at value %d", ErrTruncated, i)
		}
		out = append(out, string(data[off:off+l]))
		off += l
	}
	return out, nil
}

func encodeDictStrings(vals []string) []byte {
	index := make(map[string]uint64, 64)
	var dict []string
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
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(dict)))
	out := append([]byte(nil), hdr[:]...)
	for _, v := range dict {
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(v)))
		out = append(out, hdr[:]...)
		out = append(out, v...)
	}
	out = append(out, byte(width))
	var w bitWriter
	w.buf = out
	for _, id := range ids {
		w.write(id, uint(width))
	}
	return w.bytes()
}

func decodeDictStrings(data []byte, n int) ([]string, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("%w: string dict header", ErrTruncated)
	}
	nd := int(binary.LittleEndian.Uint32(data[:4]))
	off := 4
	dict := make([]string, nd)
	for i := 0; i < nd; i++ {
		if len(data)-off < 4 {
			return nil, fmt.Errorf("%w: string dict entry %d length", ErrTruncated, i)
		}
		l := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		if len(data)-off < l {
			return nil, fmt.Errorf("%w: string dict entry %d body", ErrTruncated, i)
		}
		dict[i] = string(data[off : off+l])
		off += l
	}
	if len(data)-off < 1 {
		return nil, fmt.Errorf("%w: string dict width", ErrTruncated)
	}
	width := uint(data[off])
	off++
	if width > 64 {
		return nil, fmt.Errorf("%w: string dict width %d", ErrCorrupt, width)
	}
	r := bitReader{buf: data[off:]}
	out := make([]string, n)
	for i := range out {
		id, ok := r.read(width)
		if !ok {
			return nil, fmt.Errorf("%w: string dict ids at value %d", ErrTruncated, i)
		}
		if id >= uint64(nd) {
			return nil, fmt.Errorf("%w: string dict id %d out of range %d", ErrCorrupt, id, nd)
		}
		out[i] = dict[id]
	}
	return out, nil
}
