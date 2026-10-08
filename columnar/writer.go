package columnar

import (
	"encoding/binary"
	"fmt"
	"os"
)

// Config controls writer behaviour.
type Config struct {
	// BlockRows is the target number of rows per block. Default 8192.
	BlockRows int
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config { return Config{BlockRows: 8192} }

func (c Config) withDefaults() Config {
	if c.BlockRows <= 0 {
		c.BlockRows = 8192
	}
	return c
}

// Writer streams rows to a file block by block. It is not safe for
// concurrent use; exactly one Writer may exist for a given path at a
// time (enforced by exclusive creation of the temporary file).
//
// Rows are written to "<path>.tmp" and atomically renamed to path on
// Close, so concurrent readers only ever see a complete file.
type Writer struct {
	path        string
	tmpPath     string
	f           *os.File
	schema      *Schema
	cfg         Config
	pending     []ColumnBatch
	pendingRows int
	blocks      []blockMeta
	offset      int64
	totalRows   int64
	closed      bool
}

// NewWriter creates a Writer writing to path. It fails with
// ErrWriterExists if another writer is active on the same path.
func NewWriter(path string, schema *Schema, cfg *Config) (*Writer, error) {
	if schema == nil || len(schema.Columns) == 0 {
		return nil, fmt.Errorf("%w: empty schema", ErrSchema)
	}
	for _, c := range schema.Columns {
		if c.Type < Int64Type || c.Type > StringType {
			return nil, fmt.Errorf("%w: column %q type %d", ErrSchema, c.Name, c.Type)
		}
	}
	c := DefaultConfig()
	if cfg != nil {
		c = cfg.withDefaults()
	}
	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_TRUNC, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrWriterExists, tmpPath)
		}
		return nil, err
	}
	w := &Writer{
		path:    path,
		tmpPath: tmpPath,
		f:       f,
		schema:  schema,
		cfg:     c,
		pending: make([]ColumnBatch, len(schema.Columns)),
	}
	hdr := encodeHeader()
	if _, err := f.Write(hdr); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	w.offset = int64(len(hdr))
	return w, nil
}

// WriteBatch appends a batch of rows. Batches may be arbitrarily large;
// rows are buffered only until one block fills, so memory stays bounded
// by roughly one block per column.
func (w *Writer) WriteBatch(b *Batch) error {
	if w.closed {
		return ErrWriterClosed
	}
	if b == nil || len(b.Cols) != len(w.schema.Columns) {
		return fmt.Errorf("%w: batch has %d columns, schema has %d", ErrSchema, len(b.Cols), len(w.schema.Columns))
	}
	n := b.Rows()
	for i := range b.Cols {
		if b.Cols[i].Rows() != n {
			return fmt.Errorf("%w: ragged batch", ErrSchema)
		}
	}
	for row := 0; row < n; {
		take := w.cfg.BlockRows - w.pendingRows
		if rem := n - row; take > rem {
			take = rem
		}
		for ci := range b.Cols {
			appendRows(&w.pending[ci], &b.Cols[ci], row, row+take, w.schema.Columns[ci].Type)
		}
		w.pendingRows += take
		row += take
		if w.pendingRows == w.cfg.BlockRows {
			if err := w.flushBlock(); err != nil {
				return err
			}
		}
	}
	w.totalRows += int64(n)
	return nil
}

// flushBlock encodes and writes the pending rows as one block.
func (w *Writer) flushBlock() error {
	if w.pendingRows == 0 {
		return nil
	}
	data, stats, err := encodeBlock(w.schema, w.pending, w.pendingRows)
	if err != nil {
		return err
	}
	if _, err := w.f.Write(data); err != nil {
		return err
	}
	w.blocks = append(w.blocks, blockMeta{
		Offset:  w.offset,
		NumRows: int64(w.pendingRows),
		Stats:   stats,
	})
	w.offset += int64(len(data))
	w.pending = make([]ColumnBatch, len(w.schema.Columns))
	w.pendingRows = 0
	return nil
}

// Close flushes pending rows, writes the footer and atomically publishes
// the file. After Close the Writer must not be reused.
func (w *Writer) Close() error {
	if w.closed {
		return ErrWriterClosed
	}
	w.closed = true
	if err := w.flushBlock(); err != nil {
		w.abort()
		return err
	}
	foot := encodeFooter(w.schema, w.blocks)
	if _, err := w.f.Write(foot); err != nil {
		w.abort()
		return err
	}
	var trailer [trailerLen]byte
	binary.LittleEndian.PutUint32(trailer[:4], uint32(len(foot)))
	copy(trailer[4:], magicTail)
	if _, err := w.f.Write(trailer[:]); err != nil {
		w.abort()
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.abort()
		return err
	}
	if err := w.f.Close(); err != nil {
		w.abort()
		return err
	}
	if err := os.Rename(w.tmpPath, w.path); err != nil {
		os.Remove(w.tmpPath)
		return err
	}
	return nil
}

// abort cleans up the temporary file after a failed write.
func (w *Writer) abort() {
	w.f.Close()
	os.Remove(w.tmpPath)
}

// appendRows appends rows [from,to) of src to dst. Value slices hold
// only non-null values, so they are sliced by counting non-nulls.
func appendRows(dst, src *ColumnBatch, from, to int, typ Type) {
	nnFrom, nnTo := 0, 0
	for i := 0; i < to; i++ {
		if !src.Nulls[i] {
			if i < from {
				nnFrom++
			}
			nnTo++
		}
	}
	dst.Nulls = append(dst.Nulls, src.Nulls[from:to]...)
	switch typ {
	case Int64Type:
		dst.Ints = append(dst.Ints, src.Ints[nnFrom:nnTo]...)
	case Float64Type:
		dst.Floats = append(dst.Floats, src.Floats[nnFrom:nnTo]...)
	case StringType:
		dst.Strs = append(dst.Strs, src.Strs[nnFrom:nnTo]...)
	}
}
