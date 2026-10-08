package columnar

import (
	"fmt"
	"io"
	"os"
)

// Reader provides random-access, projected and filtered reads over one
// file. It is safe for concurrent use by multiple goroutines: all file
// access uses ReadAt and no state is shared between Scanners.
type Reader struct {
	f         *os.File
	schema    *Schema
	blocks    []blockMeta
	totalRows int64
}

// Open opens a completed file for reading. It validates the header and
// footer; data blocks are validated lazily as they are read.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Reader, error) {
		f.Close()
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	size := fi.Size()
	if size < int64(headerLen+trailerLen) {
		return fail(fmt.Errorf("%w: file too small (%d bytes)", ErrTruncated, size))
	}
	// Trailer.
	var trailer [trailerLen]byte
	if _, err := f.ReadAt(trailer[:], size-int64(trailerLen)); err != nil {
		return fail(fmt.Errorf("%w: trailer", ErrTruncated))
	}
	if string(trailer[4:]) != magicTail {
		return fail(fmt.Errorf("%w: bad tail magic", ErrCorrupt))
	}
	footerLen := int64(readUint32(trailer[:4]))
	if footerLen <= 0 || footerLen > maxFooter || footerLen > size-int64(headerLen+trailerLen) {
		return fail(fmt.Errorf("%w: bad footer length %d", ErrCorrupt, footerLen))
	}
	// Footer.
	fbuf := make([]byte, footerLen)
	if _, err := f.ReadAt(fbuf, size-int64(trailerLen)-footerLen); err != nil {
		return fail(fmt.Errorf("%w: footer", ErrTruncated))
	}
	foot, err := parseFooter(fbuf)
	if err != nil {
		return fail(err)
	}
	// Header.
	hbuf := make([]byte, headerLen)
	if _, err := f.ReadAt(hbuf, 0); err != nil {
		return fail(fmt.Errorf("%w: header", ErrTruncated))
	}
	if err := parseHeader(hbuf); err != nil {
		return fail(err)
	}
	r := &Reader{f: f, schema: foot.schema, blocks: foot.blocks}
	for _, b := range foot.blocks {
		r.totalRows += b.NumRows
	}
	return r, nil
}

// Schema returns the file schema.
func (r *Reader) Schema() *Schema { return r.schema }

// NumRows returns the total number of rows.
func (r *Reader) NumRows() int64 { return r.totalRows }

// NumBlocks returns the number of row blocks.
func (r *Reader) NumBlocks() int { return len(r.blocks) }

// Close releases the underlying file.
func (r *Reader) Close() error { return r.f.Close() }

// readAt reads exactly len(buf) bytes at off, mapping short reads to
// ErrTruncated.
func (r *Reader) readAt(buf []byte, off int64) error {
	n, err := r.f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return err
	}
	if n != len(buf) {
		return fmt.Errorf("%w: read %d of %d bytes at %d", ErrTruncated, n, len(buf), off)
	}
	return nil
}

// Scanner streams matching batches for a projected, filtered read.
// Each Scanner is independent; different Scanners may run concurrently.
type Scanner struct {
	r       *Reader
	projIdx []int       // schema indices of projected columns
	preds   []boundPred // predicates, resolved to schema indices
	blocks  []blockMeta // blocks that passed the stats check
	next    int         // next block to decode
	cur     *Batch
	err     error
}

type boundPred struct {
	colIdx int
	p      Predicate
}

// Scan creates a Scanner over the given columns (nil = all columns, in
// schema order) and predicates (nil = match everything, AND semantics).
// Blocks whose statistics prove no row can match are never read.
func (r *Reader) Scan(cols []string, preds []Predicate) (*Scanner, error) {
	projIdx := make([]int, 0, len(r.schema.Columns))
	if cols == nil {
		for i := range r.schema.Columns {
			projIdx = append(projIdx, i)
		}
	} else {
		for _, name := range cols {
			i := r.schema.ColumnIndex(name)
			if i < 0 {
				return nil, fmt.Errorf("%w: unknown column %q", ErrSchema, name)
			}
			projIdx = append(projIdx, i)
		}
	}
	bound := make([]boundPred, 0, len(preds))
	for _, p := range preds {
		i := r.schema.ColumnIndex(p.Column)
		if i < 0 {
			return nil, fmt.Errorf("%w: unknown predicate column %q", ErrSchema, p.Column)
		}
		bound = append(bound, boundPred{colIdx: i, p: p})
	}
	s := &Scanner{r: r, projIdx: projIdx, preds: bound}
	// Block-level pruning via statistics.
	for _, b := range r.blocks {
		match := true
		for _, bp := range bound {
			ok, err := b.Stats[bp.colIdx].mayMatch(r.schema.Columns[bp.colIdx].Type, bp.p)
			if err != nil {
				return nil, err
			}
			if !ok {
				match = false
				break
			}
		}
		if match {
			s.blocks = append(s.blocks, b)
		}
	}
	return s, nil
}

// Next advances to the next batch of matching rows. It returns false at
// the end of the stream or on error (see Err).
func (s *Scanner) Next() bool {
	if s.err != nil {
		return false
	}
	for s.next < len(s.blocks) {
		b := s.blocks[s.next]
		s.next++
		batch, err := s.readBlock(b)
		if err != nil {
			s.err = err
			return false
		}
		if batch.Rows() > 0 {
			s.cur = batch
			return true
		}
	}
	s.cur = nil
	return false
}

// Batch returns the current batch; valid until the next Next call.
func (s *Scanner) Batch() *Batch { return s.cur }

// Err returns the first error encountered, if any.
func (s *Scanner) Err() error { return s.err }

// readBlock decodes the needed columns of one block and applies the
// row-level predicate filter. Only projected and predicate columns are
// read from disk and decoded.
func (s *Scanner) readBlock(b blockMeta) (*Batch, error) {
	ncol := len(s.r.schema.Columns)
	hdrLen := blockHeaderLen(ncol)
	hbuf := make([]byte, hdrLen)
	if err := s.r.readAt(hbuf, b.Offset); err != nil {
		return nil, err
	}
	hdr, err := parseBlockHeader(hbuf, b.Offset)
	if err != nil {
		return nil, err
	}
	if hdr.numRows != b.NumRows {
		return nil, fmt.Errorf("%w: block row count %d, footer says %d", ErrCorrupt, hdr.numRows, b.NumRows)
	}
	// Columns we must decode: projection ∪ predicate columns.
	needed := make([]bool, ncol)
	for _, i := range s.projIdx {
		needed[i] = true
	}
	for _, bp := range s.preds {
		needed[bp.colIdx] = true
	}
	decoded := make([]*ColumnBatch, ncol)
	for ci := 0; ci < ncol; ci++ {
		if !needed[ci] {
			continue
		}
		m := hdr.chunks[ci]
		buf := make([]byte, m.Length+4)
		if err := s.r.readAt(buf, m.Offset); err != nil {
			return nil, err
		}
		cb, err := decodeChunk(s.r.schema.Columns[ci].Type, m, buf, int(b.NumRows))
		if err != nil {
			return nil, err
		}
		decoded[ci] = &cb
	}
	// Row filter.
	nrows := int(b.NumRows)
	keep := make([]int, 0, nrows)
	if len(s.preds) == 0 {
		for i := 0; i < nrows; i++ {
			keep = append(keep, i)
		}
	} else {
		// Precompute non-null prefix indices for predicate columns.
		type predCol struct {
			bp   boundPred
			nnIx []int
		}
		pcols := make([]predCol, len(s.preds))
		for pi, bp := range s.preds {
			cb := decoded[bp.colIdx]
			nn := make([]int, nrows+1)
			for i := 0; i < nrows; i++ {
				nn[i+1] = nn[i]
				if !cb.Nulls[i] {
					nn[i+1]++
				}
			}
			pcols[pi] = predCol{bp: bp, nnIx: nn}
		}
	rowLoop:
		for i := 0; i < nrows; i++ {
			for _, pc := range pcols {
				cb := decoded[pc.bp.colIdx]
				typ := s.r.schema.Columns[pc.bp.colIdx].Type
				if !matchRow(typ, pc.bp.p, cb, i, pc.nnIx[i]) {
					continue rowLoop
				}
			}
			keep = append(keep, i)
		}
	}
	// Build the output batch with projected columns only.
	out := &Batch{Cols: make([]ColumnBatch, len(s.projIdx))}
	for oi, ci := range s.projIdx {
		src := decoded[ci]
		typ := s.r.schema.Columns[ci].Type
		dst := &out.Cols[oi]
		dst.Nulls = make([]bool, len(keep))
		nn := 0
		for _, row := range keep {
			dst.Nulls[nn] = src.Nulls[row]
			nn++
		}
		// Collect values of kept non-null rows.
		nnIx := make([]int, nrows+1)
		for i := 0; i < nrows; i++ {
			nnIx[i+1] = nnIx[i]
			if !src.Nulls[i] {
				nnIx[i+1]++
			}
		}
		switch typ {
		case Int64Type:
			for _, row := range keep {
				if !src.Nulls[row] {
					dst.Ints = append(dst.Ints, src.Ints[nnIx[row]])
				}
			}
		case Float64Type:
			for _, row := range keep {
				if !src.Nulls[row] {
					dst.Floats = append(dst.Floats, src.Floats[nnIx[row]])
				}
			}
		case StringType:
			for _, row := range keep {
				if !src.Nulls[row] {
					dst.Strs = append(dst.Strs, src.Strs[nnIx[row]])
				}
			}
		}
	}
	return out, nil
}

// matchRow evaluates one predicate against row i of a decoded column.
// nnIx is the number of non-null values before row i.
func matchRow(typ Type, p Predicate, cb *ColumnBatch, row, nnIx int) bool {
	if cb.Nulls[row] {
		return p.Op == IsNull
	}
	if p.Op == IsNull {
		return false
	}
	if p.Op == NotNull {
		return true
	}
	var v any
	switch typ {
	case Int64Type:
		v = cb.Ints[nnIx]
	case Float64Type:
		v = cb.Floats[nnIx]
	case StringType:
		v = cb.Strs[nnIx]
	}
	c := compareValues(typ, v, p.Value)
	switch p.Op {
	case Eq:
		return c == 0
	case Ne:
		return c != 0
	case Lt:
		return c < 0
	case Le:
		return c <= 0
	case Gt:
		return c > 0
	case Ge:
		return c >= 0
	}
	return false
}

func readUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
