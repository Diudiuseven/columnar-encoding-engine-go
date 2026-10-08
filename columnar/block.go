package columnar

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

// blockMeta describes one row block; stored in the footer.
type blockMeta struct {
	Offset  int64 // absolute offset of the block on disk
	NumRows int64
	Stats   []Stats // per column, in schema order
}

// Block on-disk layout:
//
//	numRows(4) numCols(2)
//	per column: encodingID(1) encodingVersion(1) chunkLen(4)
//	headerCRC(4) over the header bytes above
//	per column: chunk payload, then crc32c(4) of the payload
//
// The header and every chunk are checksummed independently, so a reader
// can fetch and verify only the column chunks it needs.
//
// Chunk payload layout:
//
//	nullBitmapLen(4) nullBitmap nullValuePayload
//
// The null bitmap has one bit per row (LSB-first); it is omitted
// (length 0) when the chunk has no nulls. The value payload holds only
// the non-null values, encoded with the chunk's encoding.

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// encodeBlock encodes one block of rows. cols must have one entry per
// schema column, each with exactly numRows rows. It returns the block
// bytes (including CRC) and per-column statistics.
func encodeBlock(schema *Schema, cols []ColumnBatch, numRows int) ([]byte, []Stats, error) {
	if len(cols) != len(schema.Columns) {
		return nil, nil, fmt.Errorf("%w: %d column batches for %d columns", ErrSchema, len(cols), len(schema.Columns))
	}
	ncol := len(schema.Columns)
	type encChunk struct {
		id      EncodingID
		payload []byte
	}
	chunks := make([]encChunk, ncol)
	stats := make([]Stats, ncol)
	for ci, col := range schema.Columns {
		cb := &cols[ci]
		if cb.Rows() != numRows {
			return nil, nil, fmt.Errorf("%w: column %q has %d rows, want %d", ErrSchema, col.Name, cb.Rows(), numRows)
		}
		stats[ci] = computeStats(col.Type, cb)
		nulls := encodeNullBitmap(cb.Nulls, int(stats[ci].NullCount))
		var id EncodingID
		var payload []byte
		var err error
		switch col.Type {
		case Int64Type:
			if int64(len(cb.Ints)) != stats[ci].NumRows-stats[ci].NullCount {
				return nil, nil, fmt.Errorf("%w: column %q value count", ErrSchema, col.Name)
			}
			id = chooseIntEncoding(cb.Ints)
			payload, err = encodeInts(id, cb.Ints)
		case Float64Type:
			if int64(len(cb.Floats)) != stats[ci].NumRows-stats[ci].NullCount {
				return nil, nil, fmt.Errorf("%w: column %q value count", ErrSchema, col.Name)
			}
			bits := make([]int64, len(cb.Floats))
			for i, f := range cb.Floats {
				bits[i] = int64(math.Float64bits(f))
			}
			id = chooseIntEncoding(bits)
			payload, err = encodeInts(id, bits)
		case StringType:
			if int64(len(cb.Strs)) != stats[ci].NumRows-stats[ci].NullCount {
				return nil, nil, fmt.Errorf("%w: column %q value count", ErrSchema, col.Name)
			}
			id = chooseStringEncoding(cb.Strs)
			payload, err = encodeStrings(id, cb.Strs)
		default:
			return nil, nil, fmt.Errorf("%w: column %q type %d", ErrSchema, col.Name, col.Type)
		}
		if err != nil {
			return nil, nil, err
		}
		var nb [4]byte
		binary.LittleEndian.PutUint32(nb[:], uint32(len(nulls)))
		full := make([]byte, 0, 4+len(nulls)+len(payload))
		full = append(full, nb[:]...)
		full = append(full, nulls...)
		full = append(full, payload...)
		chunks[ci] = encChunk{id: id, payload: full}
	}
	// Header.
	hdrLen := blockHeaderLen(ncol)
	out := make([]byte, 0, hdrLen+64)
	var hb [6]byte
	binary.LittleEndian.PutUint32(hb[:4], uint32(numRows))
	binary.LittleEndian.PutUint16(hb[4:], uint16(ncol))
	out = append(out, hb[:]...)
	for _, ch := range chunks {
		out = append(out, byte(ch.id), encVersion)
		var lb [4]byte
		binary.LittleEndian.PutUint32(lb[:], uint32(len(ch.payload)))
		out = append(out, lb[:]...)
	}
	var cb4 [4]byte
	binary.LittleEndian.PutUint32(cb4[:], crc32.Checksum(out, crcTable))
	out = append(out, cb4[:]...)
	// Chunks, each followed by its own CRC.
	for _, ch := range chunks {
		out = append(out, ch.payload...)
		binary.LittleEndian.PutUint32(cb4[:], crc32.Checksum(ch.payload, crcTable))
		out = append(out, cb4[:]...)
	}
	return out, stats, nil
}

// blockHeaderLen returns the encoded length of a block header.
func blockHeaderLen(ncol int) int { return 6 + ncol*6 + 4 }

// blockHeader is the parsed header of an on-disk block.
type blockHeader struct {
	numRows int64
	chunks  []chunkMeta
}

// chunkMeta locates one column chunk payload within the file.
type chunkMeta struct {
	Encoding   EncodingID
	EncVersion byte
	Offset     int64
	Length     int64
}

// parseBlockHeader validates a block header read from disk (data must
// be exactly blockHeaderLen bytes) and returns the chunk index. Chunk
// offsets are absolute; each chunk on disk is payload plus a 4-byte CRC.
func parseBlockHeader(data []byte, blockOffset int64) (*blockHeader, error) {
	if len(data) < 6+4 {
		return nil, fmt.Errorf("%w: block header", ErrTruncated)
	}
	numRows := int64(binary.LittleEndian.Uint32(data[:4]))
	ncol := int(binary.LittleEndian.Uint16(data[4:]))
	hdrLen := blockHeaderLen(ncol)
	if len(data) < hdrLen {
		return nil, fmt.Errorf("%w: block column table", ErrTruncated)
	}
	stored := binary.LittleEndian.Uint32(data[hdrLen-4:])
	if got := crc32.Checksum(data[:hdrLen-4], crcTable); got != stored {
		return nil, fmt.Errorf("%w: block header checksum mismatch", ErrCorrupt)
	}
	h := &blockHeader{numRows: numRows, chunks: make([]chunkMeta, ncol)}
	off := int64(hdrLen)
	for i := 0; i < ncol; i++ {
		base := 6 + i*6
		l := int64(binary.LittleEndian.Uint32(data[base+2:]))
		h.chunks[i] = chunkMeta{
			Encoding:   EncodingID(data[base]),
			EncVersion: data[base+1],
			Offset:     blockOffset + off,
			Length:     l,
		}
		off += l + 4
	}
	return h, nil
}

// decodeChunk decodes one column chunk into a ColumnBatch. payload must
// be the on-disk chunk bytes: encoded payload plus its trailing CRC.
func decodeChunk(typ Type, m chunkMeta, payload []byte, numRows int) (ColumnBatch, error) {
	if len(payload) < 4 {
		return ColumnBatch{}, fmt.Errorf("%w: chunk crc", ErrTruncated)
	}
	stored := binary.LittleEndian.Uint32(payload[len(payload)-4:])
	if got := crc32.Checksum(payload[:len(payload)-4], crcTable); got != stored {
		return ColumnBatch{}, fmt.Errorf("%w: chunk checksum mismatch", ErrCorrupt)
	}
	payload = payload[:len(payload)-4]
	if len(payload) < 4 {
		return ColumnBatch{}, fmt.Errorf("%w: chunk null header", ErrTruncated)
	}
	nullLen := int(binary.LittleEndian.Uint32(payload[:4]))
	if len(payload)-4 < nullLen {
		return ColumnBatch{}, fmt.Errorf("%w: chunk null bitmap", ErrTruncated)
	}
	nulls, nullCount, err := decodeNullBitmap(payload[4:4+nullLen], numRows)
	if err != nil {
		return ColumnBatch{}, err
	}
	nvals := numRows - nullCount
	body := payload[4+nullLen:]
	cb := ColumnBatch{Nulls: nulls}
	switch typ {
	case Int64Type:
		vs, err := decodeInts(m.Encoding, m.EncVersion, body, nvals)
		if err != nil {
			return ColumnBatch{}, err
		}
		cb.Ints = vs
	case Float64Type:
		vs, err := decodeInts(m.Encoding, m.EncVersion, body, nvals)
		if err != nil {
			return ColumnBatch{}, err
		}
		cb.Floats = make([]float64, len(vs))
		for i, b := range vs {
			cb.Floats[i] = math.Float64frombits(uint64(b))
		}
	case StringType:
		vs, err := decodeStrings(m.Encoding, m.EncVersion, body, nvals)
		if err != nil {
			return ColumnBatch{}, err
		}
		cb.Strs = vs
	default:
		return ColumnBatch{}, fmt.Errorf("%w: column type %d", ErrCorrupt, typ)
	}
	return cb, nil
}

// encodeNullBitmap packs the null flags LSB-first. Returns nil when
// there are no nulls.
func encodeNullBitmap(nulls []bool, nullCount int) []byte {
	if nullCount == 0 {
		return nil
	}
	out := make([]byte, (len(nulls)+7)/8)
	for i, n := range nulls {
		if n {
			out[i>>3] |= 1 << (uint(i) & 7)
		}
	}
	return out
}

func decodeNullBitmap(data []byte, numRows int) ([]bool, int, error) {
	nulls := make([]bool, numRows)
	if len(data) == 0 {
		return nulls, 0, nil
	}
	if len(data) != (numRows+7)/8 {
		return nil, 0, fmt.Errorf("%w: null bitmap has %d bytes, want %d", ErrCorrupt, len(data), (numRows+7)/8)
	}
	cnt := 0
	for i := 0; i < numRows; i++ {
		if data[i>>3]&(1<<(uint(i)&7)) != 0 {
			nulls[i] = true
			cnt++
		}
	}
	return nulls, cnt, nil
}
