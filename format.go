package columnar

import (
	"fmt"
	"hash/crc32"
)

// On-disk layout:
//
//   header:  magic(4) | major(1) | minor(1) | reserved(2)
//   blocks:  sequence of chunks; each chunk is one encoded column of one
//            block. Chunk bytes are:
//              uvarint nullBitmapLen | null bitmap | encoded values
//            optionally flate-compressed (flag in directory entry).
//   footer:  directory | dirLen u32 LE | dirCRC32 u32 LE | magic(4)
//
// The directory describes the schema and every chunk (offset, length,
// encoding, encoding version, compression, CRC32, stats). All integers
// are little-endian or uvarint; no timestamps or maps are serialized, so
// output is byte-reproducible for identical input and configuration.

var fileMagic = []byte{'C', 'E', 'N', 'R'}

const (
	formatMajor uint8 = 1
	formatMinor uint8 = 0
	headerSize        = 8
	trailerSize       = 12 // dirLen(4) + dirCRC(4) + magic(4)
)

// Compression identifies the chunk compression algorithm.
type Compression uint8

const (
	CompNone  Compression = 0
	CompFlate Compression = 1
)

// chunkMeta is the directory entry for one encoded column chunk.
type chunkMeta struct {
	Encoding    Encoding
	EncVersion  uint8
	Compression Compression
	Offset      uint64 // absolute file offset of the chunk
	StoredLen   uint64 // bytes on disk (compressed)
	RawLen      uint64 // bytes before compression
	CRC32       uint32 // CRC-32 (IEEE) of the stored bytes
	Stats       BlockStats
}

// blockMeta describes one row block.
type blockMeta struct {
	RowCount uint32
	Chunks   []chunkMeta // one per column, in schema order
}

// directory is the in-memory footer.
type directory struct {
	Schema Schema
	Blocks []blockMeta
}

// encodeHeader returns the 8-byte file header.
func encodeHeader() []byte {
	h := make([]byte, 0, headerSize)
	h = append(h, fileMagic...)
	h = append(h, formatMajor, formatMinor, 0, 0)
	return h
}

// encodeDirectory serializes d deterministically.
func encodeDirectory(d *directory) []byte {
	w := &byteWriter{}
	w.uvarint(uint64(len(d.Schema.Columns)))
	for _, c := range d.Schema.Columns {
		w.uvarint(uint64(len(c.Name)))
		w.bytes([]byte(c.Name))
		w.u8(uint8(c.Type))
	}
	w.uvarint(uint64(len(d.Blocks)))
	for _, b := range d.Blocks {
		w.uvarint(uint64(b.RowCount))
		for i := range b.Chunks {
			ch := &b.Chunks[i]
			w.u8(uint8(ch.Encoding))
			w.u8(ch.EncVersion)
			w.u8(uint8(ch.Compression))
			w.uvarint(ch.Offset)
			w.uvarint(ch.StoredLen)
			w.uvarint(ch.RawLen)
			var crcb [4]byte
			crcb[0] = byte(ch.CRC32)
			crcb[1] = byte(ch.CRC32 >> 8)
			crcb[2] = byte(ch.CRC32 >> 16)
			crcb[3] = byte(ch.CRC32 >> 24)
			w.bytes(crcb[:])
			encodeStats(w, &ch.Stats)
		}
	}
	return w.buf
}

func encodeStats(w *byteWriter, s *BlockStats) {
	w.uvarint(uint64(s.RowCount))
	w.uvarint(uint64(s.NullCount))
	if s.HasMinMax {
		w.u8(1)
		plainEncodeValue(w, s.Min)
		plainEncodeValue(w, s.Max)
	} else {
		w.u8(0)
	}
}

func decodeStats(r *byteReader, kind Kind) (BlockStats, error) {
	var s BlockStats
	s.RowCount = uint32(r.uvarint())
	s.NullCount = uint32(r.uvarint())
	if r.err != nil {
		return s, r.err
	}
	if s.NullCount > s.RowCount {
		return s, fmt.Errorf("%w: null count %d exceeds row count %d",
			ErrCorrupt, s.NullCount, s.RowCount)
	}
	if r.u8() == 1 {
		s.HasMinMax = true
		s.Min = plainDecodeValue(r, kind)
		if r.err != nil {
			return s, r.err
		}
		s.Max = plainDecodeValue(r, kind)
		if r.err != nil {
			return s, r.err
		}
		if compareValues(s.Min, s.Max) > 0 {
			return s, fmt.Errorf("%w: stats min greater than max", ErrCorrupt)
		}
	}
	return s, nil
}

// decodeDirectory parses the directory bytes.
func decodeDirectory(data []byte) (*directory, error) {
	r := &byteReader{buf: data}
	d := &directory{}
	ncols := r.uvarint()
	if r.err != nil {
		return nil, r.err
	}
	if ncols > 1<<20 {
		return nil, fmt.Errorf("%w: implausible column count %d", ErrCorrupt, ncols)
	}
	for i := uint64(0); i < ncols; i++ {
		nlen := r.uvarint()
		if r.err != nil {
			return nil, r.err
		}
		if nlen > 1<<20 {
			return nil, fmt.Errorf("%w: implausible column name length %d", ErrCorrupt, nlen)
		}
		name := r.bytes(int(nlen))
		if name == nil {
			return nil, r.err
		}
		kind := Kind(r.u8())
		if r.err != nil {
			return nil, r.err
		}
		if kind == KindInvalid || kind > KindBool {
			return nil, fmt.Errorf("%w: invalid column kind %d", ErrCorrupt, kind)
		}
		d.Schema.Columns = append(d.Schema.Columns, Column{Name: string(name), Type: kind})
	}
	if err := d.Schema.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	nblocks := r.uvarint()
	if r.err != nil {
		return nil, r.err
	}
	if nblocks > 1<<31 {
		return nil, fmt.Errorf("%w: implausible block count %d", ErrCorrupt, nblocks)
	}
	for b := uint64(0); b < nblocks; b++ {
		bm := blockMeta{RowCount: uint32(r.uvarint())}
		if r.err != nil {
			return nil, r.err
		}
		for c := range d.Schema.Columns {
			var ch chunkMeta
			ch.Encoding = Encoding(r.u8())
			ch.EncVersion = r.u8()
			ch.Compression = Compression(r.u8())
			ch.Offset = r.uvarint()
			ch.StoredLen = r.uvarint()
			ch.RawLen = r.uvarint()
			if r.err != nil {
				return nil, r.err
			}
			crcb := r.bytes(4)
			if crcb == nil {
				return nil, r.err
			}
			ch.CRC32 = uint32(crcb[0]) | uint32(crcb[1])<<8 | uint32(crcb[2])<<16 | uint32(crcb[3])<<24
			st, err := decodeStats(r, d.Schema.Columns[c].Type)
			if err != nil {
				return nil, err
			}
			ch.Stats = st
			bm.Chunks = append(bm.Chunks, ch)
		}
		d.Blocks = append(d.Blocks, bm)
	}
	if err := r.finish(); err != nil {
		return nil, err
	}
	return d, nil
}

// crc32Of computes the CRC-32 (IEEE) used for chunk and directory
// integrity.
func crc32Of(b []byte) uint32 { return crc32.ChecksumIEEE(b) }
