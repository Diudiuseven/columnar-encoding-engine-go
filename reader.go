package columnar

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"os"
)

// Reader provides random access to an encoded file. It is safe for
// concurrent use by multiple goroutines; every method is read-only and
// chunk reads go through io.ReaderAt.
type Reader struct {
	f   *os.File
	dir *directory
}

// Open opens an encoded file for reading. It validates the header,
// trailer, directory CRC and chunk bounds, but does not decode any chunk.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r, err := openReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return r, nil
}

func openReader(f *os.File) (*Reader, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size < int64(headerSize+trailerSize) {
		return nil, fmt.Errorf("%w: file too small (%d bytes)", ErrCorrupt, size)
	}

	hdr := make([]byte, headerSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return nil, fmt.Errorf("%w: reading header: %v", ErrCorrupt, err)
	}
	if !bytes.Equal(hdr[:4], fileMagic) {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if hdr[4] > formatMajor {
		return nil, fmt.Errorf("%w: file format major %d > supported %d",
			ErrUnsupportedVersion, hdr[4], formatMajor)
	}

	trailer := make([]byte, trailerSize)
	if _, err := f.ReadAt(trailer, size-int64(trailerSize)); err != nil {
		return nil, fmt.Errorf("%w: reading trailer: %v", ErrCorrupt, err)
	}
	if !bytes.Equal(trailer[8:], fileMagic) {
		return nil, fmt.Errorf("%w: bad trailer magic (truncated file?)", ErrCorrupt)
	}
	dirLen := uint64(trailer[0]) | uint64(trailer[1])<<8 | uint64(trailer[2])<<16 | uint64(trailer[3])<<24
	dirCRC := uint32(trailer[4]) | uint32(trailer[5])<<8 | uint32(trailer[6])<<16 | uint32(trailer[7])<<24
	if dirLen == 0 || dirLen > uint64(size)-headerSize-trailerSize {
		return nil, fmt.Errorf("%w: directory length %d out of bounds", ErrCorrupt, dirLen)
	}
	dirOff := size - int64(trailerSize) - int64(dirLen)
	if dirOff < headerSize {
		return nil, fmt.Errorf("%w: directory overlaps header", ErrCorrupt)
	}
	dirBytes := make([]byte, dirLen)
	if _, err := f.ReadAt(dirBytes, dirOff); err != nil {
		return nil, fmt.Errorf("%w: reading directory: %v", ErrCorrupt, err)
	}
	if crc32Of(dirBytes) != dirCRC {
		return nil, fmt.Errorf("%w: directory CRC mismatch", ErrCorrupt)
	}
	dir, err := decodeDirectory(dirBytes)
	if err != nil {
		return nil, err
	}
	// Validate chunk bounds eagerly so corrupt metadata is caught at open.
	for bi := range dir.Blocks {
		for ci := range dir.Blocks[bi].Chunks {
			ch := &dir.Blocks[bi].Chunks[ci]
			if ch.Offset < headerSize || ch.Offset+ch.StoredLen > uint64(dirOff) {
				return nil, fmt.Errorf("%w: chunk (%d,%d) out of bounds", ErrCorrupt, bi, ci)
			}
		}
	}
	return &Reader{f: f, dir: dir}, nil
}

// Close closes the underlying file.
func (r *Reader) Close() error { return r.f.Close() }

// Schema returns the file schema.
func (r *Reader) Schema() Schema { return r.dir.Schema }

// NumBlocks returns the number of row blocks.
func (r *Reader) NumBlocks() int { return len(r.dir.Blocks) }

// BlockRows returns the number of rows in a block.
func (r *Reader) BlockRows(block int) (int, error) {
	if block < 0 || block >= len(r.dir.Blocks) {
		return 0, fmt.Errorf("columnar: block %d out of range", block)
	}
	return int(r.dir.Blocks[block].RowCount), nil
}

// Stats returns the statistics of one column chunk.
func (r *Reader) Stats(block, col int) (BlockStats, error) {
	ch, err := r.chunkMeta(block, col)
	if err != nil {
		return BlockStats{}, err
	}
	return ch.Stats, nil
}

func (r *Reader) chunkMeta(block, col int) (*chunkMeta, error) {
	if block < 0 || block >= len(r.dir.Blocks) {
		return nil, fmt.Errorf("columnar: block %d out of range", block)
	}
	if col < 0 || col >= r.dir.Schema.NumCols() {
		return nil, fmt.Errorf("columnar: column %d out of range", col)
	}
	return &r.dir.Blocks[block].Chunks[col], nil
}

// MayMatch returns the indices of blocks that may contain a row matching
// pred, based on block statistics. Blocks that definitely cannot match
// are excluded; excluded blocks are guaranteed to contain no match.
func (r *Reader) MayMatch(pred Predicate) ([]int, error) {
	ci := r.dir.Schema.ColIndex(pred.Column)
	if ci < 0 {
		return nil, fmt.Errorf("%w: unknown column %q", ErrSchema, pred.Column)
	}
	var out []int
	for b := range r.dir.Blocks {
		ok, err := r.dir.Blocks[b].Chunks[ci].Stats.mayMatch(pred)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// ReadBlockColumn decodes one column of one block. Only that chunk is
// loaded into memory.
func (r *Reader) ReadBlockColumn(block, col int) ([]Value, error) {
	ch, err := r.chunkMeta(block, col)
	if err != nil {
		return nil, err
	}
	rows := int(r.dir.Blocks[block].RowCount)
	kind := r.dir.Schema.Columns[col].Type

	stored := make([]byte, ch.StoredLen)
	if _, err := r.f.ReadAt(stored, int64(ch.Offset)); err != nil {
		return nil, fmt.Errorf("%w: reading chunk (%d,%d): %v", ErrCorrupt, block, col, err)
	}
	if crc32Of(stored) != ch.CRC32 {
		return nil, fmt.Errorf("%w: chunk (%d,%d) CRC mismatch", ErrCorrupt, block, col)
	}
	raw, err := decompress(stored, ch)
	if err != nil {
		return nil, fmt.Errorf("chunk (%d,%d): %w", block, col, err)
	}
	return decodeChunk(raw, rows, ch, kind)
}

// decompress reverses chunk compression, verifying the raw length.
func decompress(stored []byte, ch *chunkMeta) ([]byte, error) {
	switch ch.Compression {
	case CompNone:
		if uint64(len(stored)) != ch.RawLen {
			return nil, fmt.Errorf("%w: raw length mismatch", ErrCorrupt)
		}
		return stored, nil
	case CompFlate:
		zr := flate.NewReader(bytes.NewReader(stored))
		defer zr.Close()
		// Read at most RawLen+1 bytes to bound memory on corrupt input.
		raw, err := io.ReadAll(io.LimitReader(zr, int64(ch.RawLen)+1))
		if err != nil {
			return nil, fmt.Errorf("%w: flate decode: %v", ErrCorrupt, err)
		}
		if uint64(len(raw)) != ch.RawLen {
			return nil, fmt.Errorf("%w: decompressed length %d != expected %d",
				ErrCorrupt, len(raw), ch.RawLen)
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("%w: compression %d", ErrUnsupportedEncoding, uint8(ch.Compression))
	}
}

// decodeChunk splits the null bitmap from the encoded values and
// reconstructs the full value sequence including nulls.
func decodeChunk(raw []byte, rows int, ch *chunkMeta, kind Kind) ([]Value, error) {
	br := &byteReader{buf: raw}
	bitmapLen := br.uvarint()
	if br.err != nil {
		return nil, br.err
	}
	wantBitmap := uint64((rows + 7) / 8)
	if bitmapLen != wantBitmap {
		return nil, fmt.Errorf("%w: null bitmap length %d, want %d",
			ErrCorrupt, bitmapLen, wantBitmap)
	}
	bitmap := br.bytes(int(bitmapLen))
	if bitmap == nil {
		return nil, br.err
	}
	nulls := 0
	for i := 0; i < rows; i++ {
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			nulls++
		}
	}
	nonNull, err := decodeValues(ch.Encoding, ch.EncVersion, raw[br.off:], rows-nulls, kind)
	if err != nil {
		return nil, err
	}
	out := make([]Value, 0, rows)
	ni := 0
	for i := 0; i < rows; i++ {
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			out = append(out, NullOf(kind))
		} else {
			out = append(out, nonNull[ni])
			ni++
		}
	}
	return out, nil
}

// ReadBlock decodes the given columns of one block. cols are column
// indices; the result has one []Value per requested column. Only the
// requested chunks are read and decoded.
func (r *Reader) ReadBlock(block int, cols []int) ([][]Value, error) {
	out := make([][]Value, len(cols))
	for i, c := range cols {
		vals, err := r.ReadBlockColumn(block, c)
		if err != nil {
			return nil, err
		}
		out[i] = vals
	}
	return out, nil
}
