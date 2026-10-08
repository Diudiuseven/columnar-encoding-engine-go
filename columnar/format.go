package columnar

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

// File layout:
//
//	[magic(4) formatVersion(2)]
//	[block 0][block 1]...[block N]        (see block.go)
//	[footer][footerLen(4)][magicTail(4)]
//
// Footer layout:
//
//	numCols(2) per column: nameLen(2) name type(1)
//	numBlocks(4)
//	per block: offset(8) numRows(8)
//	  per column: hasMinMax(1) nullCount(8) [min max]   (min/max typed:
//	  int64=8B LE, float64=8B LE bits, string=len(4)+bytes)
//	crc32c(4) over all preceding footer bytes
//
// The footer carries the schema and per-block, per-column statistics so
// readers can skip blocks without touching block data.

const (
	magicHeader = "CENG"
	magicTail   = "GNE1"
	// formatVersion is the on-disk format version written by this build.
	formatVersion uint16 = 1
	// maxSupportedVersion is the highest format version this build reads.
	maxSupportedVersion uint16 = 1

	headerLen   = 6
	trailerLen  = 8 // footerLen(4) + magicTail(4)
	maxFooter   = 1 << 30
	maxBlockCnt = 1 << 30
)

func encodeHeader() []byte {
	out := make([]byte, headerLen)
	copy(out, magicHeader)
	binary.LittleEndian.PutUint16(out[4:], formatVersion)
	return out
}

// parseHeader validates the file header read from the first headerLen
// bytes.
func parseHeader(data []byte) error {
	if len(data) < headerLen {
		return fmt.Errorf("%w: file header", ErrTruncated)
	}
	if string(data[:4]) != magicHeader {
		return fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	ver := binary.LittleEndian.Uint16(data[4:])
	if ver == 0 || ver > maxSupportedVersion {
		return fmt.Errorf("%w: format version %d", ErrUnsupportedVersion, ver)
	}
	return nil
}

// footer is the parsed file footer.
type footer struct {
	schema *Schema
	blocks []blockMeta
}

func encodeFooter(schema *Schema, blocks []blockMeta) []byte {
	out := make([]byte, 0, 256)
	var tmp [8]byte
	binary.LittleEndian.PutUint16(tmp[:2], uint16(len(schema.Columns)))
	out = append(out, tmp[:2]...)
	for _, c := range schema.Columns {
		binary.LittleEndian.PutUint16(tmp[:2], uint16(len(c.Name)))
		out = append(out, tmp[:2]...)
		out = append(out, c.Name...)
		out = append(out, byte(c.Type))
	}
	binary.LittleEndian.PutUint32(tmp[:4], uint32(len(blocks)))
	out = append(out, tmp[:4]...)
	for _, b := range blocks {
		binary.LittleEndian.PutUint64(tmp[:8], uint64(b.Offset))
		out = append(out, tmp[:8]...)
		binary.LittleEndian.PutUint64(tmp[:8], uint64(b.NumRows))
		out = append(out, tmp[:8]...)
		for ci, st := range b.Stats {
			if st.Min == nil {
				out = append(out, 0)
			} else {
				out = append(out, 1)
			}
			binary.LittleEndian.PutUint64(tmp[:8], uint64(st.NullCount))
			out = append(out, tmp[:8]...)
			if st.Min == nil {
				continue
			}
			switch schema.Columns[ci].Type {
			case Int64Type:
				binary.LittleEndian.PutUint64(tmp[:8], uint64(st.Min.(int64)))
				out = append(out, tmp[:8]...)
				binary.LittleEndian.PutUint64(tmp[:8], uint64(st.Max.(int64)))
				out = append(out, tmp[:8]...)
			case Float64Type:
				binary.LittleEndian.PutUint64(tmp[:8], math.Float64bits(st.Min.(float64)))
				out = append(out, tmp[:8]...)
				binary.LittleEndian.PutUint64(tmp[:8], math.Float64bits(st.Max.(float64)))
				out = append(out, tmp[:8]...)
			case StringType:
				binary.LittleEndian.PutUint32(tmp[:4], uint32(len(st.Min.(string))))
				out = append(out, tmp[:4]...)
				out = append(out, st.Min.(string)...)
				binary.LittleEndian.PutUint32(tmp[:4], uint32(len(st.Max.(string))))
				out = append(out, tmp[:4]...)
				out = append(out, st.Max.(string)...)
			}
		}
	}
	var cb [4]byte
	binary.LittleEndian.PutUint32(cb[:], crc32.Checksum(out, crcTable))
	out = append(out, cb[:]...)
	return out
}

// parseFooter parses footer bytes (without the trailing footerLen+magic).
func parseFooter(data []byte) (*footer, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("%w: footer", ErrTruncated)
	}
	stored := binary.LittleEndian.Uint32(data[len(data)-4:])
	if got := crc32.Checksum(data[:len(data)-4], crcTable); got != stored {
		return nil, fmt.Errorf("%w: footer checksum mismatch", ErrCorrupt)
	}
	p := &byteParser{data: data[:len(data)-4]}
	ncol := int(p.u16())
	if p.err != nil || ncol > 1<<16 {
		return nil, fmt.Errorf("%w: footer column count", ErrCorrupt)
	}
	schema := &Schema{Columns: make([]Column, ncol)}
	for i := range schema.Columns {
		nameLen := int(p.u16())
		name := string(p.take(nameLen))
		typ := Type(p.u8())
		if p.err != nil {
			return nil, fmt.Errorf("%w: footer schema", ErrTruncated)
		}
		if typ < Int64Type || typ > StringType {
			return nil, fmt.Errorf("%w: unknown column type %d", ErrCorrupt, typ)
		}
		schema.Columns[i] = Column{Name: name, Type: typ}
	}
	nblocks := int64(p.u32())
	if p.err != nil || nblocks > maxBlockCnt {
		return nil, fmt.Errorf("%w: footer block count", ErrCorrupt)
	}
	f := &footer{schema: schema, blocks: make([]blockMeta, 0, nblocks)}
	for bi := int64(0); bi < nblocks; bi++ {
		off := int64(p.u64())
		nrows := int64(p.u64())
		if p.err != nil {
			return nil, fmt.Errorf("%w: footer block %d", ErrTruncated, bi)
		}
		bm := blockMeta{Offset: off, NumRows: nrows, Stats: make([]Stats, ncol)}
		for ci := range bm.Stats {
			has := p.u8()
			nc := int64(p.u64())
			if p.err != nil {
				return nil, fmt.Errorf("%w: footer stats", ErrTruncated)
			}
			if has > 1 {
				return nil, fmt.Errorf("%w: bad stats flag %d", ErrCorrupt, has)
			}
			st := Stats{NullCount: nc, NumRows: nrows}
			if has == 1 {
				switch schema.Columns[ci].Type {
				case Int64Type:
					st.Min = int64(p.u64())
					st.Max = int64(p.u64())
				case Float64Type:
					st.Min = math.Float64frombits(p.u64())
					st.Max = math.Float64frombits(p.u64())
				case StringType:
					st.Min = p.str()
					st.Max = p.str()
				}
				if p.err != nil {
					return nil, fmt.Errorf("%w: footer min/max", ErrTruncated)
				}
			}
			bm.Stats[ci] = st
		}
		f.blocks = append(f.blocks, bm)
	}
	if p.off != len(p.data) {
		return nil, fmt.Errorf("%w: footer has %d trailing bytes", ErrCorrupt, len(p.data)-p.off)
	}
	return f, nil
}

// byteParser is a bounds-checked cursor over a byte slice; it never
// panics and records the first out-of-bounds access in err.
type byteParser struct {
	data []byte
	off  int
	err  error
}

func (p *byteParser) take(n int) []byte {
	if p.err != nil {
		return nil
	}
	if len(p.data)-p.off < n {
		p.err = ErrTruncated
		return nil
	}
	b := p.data[p.off : p.off+n]
	p.off += n
	return b
}

func (p *byteParser) u8() uint8 {
	b := p.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (p *byteParser) u16() uint16 {
	b := p.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (p *byteParser) u32() uint32 {
	b := p.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (p *byteParser) u64() uint64 {
	b := p.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (p *byteParser) str() string {
	l := int(p.u32())
	if p.err != nil {
		return ""
	}
	b := p.take(l)
	if b == nil {
		return ""
	}
	return string(b)
}
