package columnar

import (
	"bytes"
	"compress/flate"
	"fmt"
	"os"
)

// DefaultBlockRows is the default number of rows per block.
const DefaultBlockRows = 8192

// Options configures a Writer. The zero value is valid.
type Options struct {
	// BlockRows is the number of rows buffered per block before it is
	// encoded and flushed. Defaults to DefaultBlockRows.
	BlockRows int
	// Compression selects chunk compression. Defaults to CompNone.
	Compression Compression
}

func (o *Options) withDefaults() Options {
	out := *o
	if out.BlockRows <= 0 {
		out.BlockRows = DefaultBlockRows
	}
	if out.Compression != CompNone && out.Compression != CompFlate {
		out.Compression = CompNone
	}
	return out
}

// Writer streams rows into an encoded file. It is not safe for concurrent
// use; exactly one Writer may target a given path at a time (enforced via
// an exclusive temporary file). Readers only see the file after Close
// atomically renames it into place.
type Writer struct {
	f         *os.File
	tmpPath   string
	finalPath string
	schema    Schema
	opts      Options

	colBufs [][]Value      // pending non-block-aligned values per column
	stats   []statsBuilder // stats for the pending block
	pending int            // rows buffered so far

	dir    *directory
	offset uint64
	closed bool
	failed bool
}

// NewWriter creates a Writer that will produce path on Close. While the
// Writer is active, path+".tmp" holds the in-progress data; a second
// NewWriter for the same path fails with ErrWriterExists.
func NewWriter(path string, schema Schema, opts *Options) (*Writer, error) {
	if err := schema.validate(); err != nil {
		return nil, err
	}
	o := Options{}
	if opts != nil {
		o = *opts
	}
	o = o.withDefaults()

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrWriterExists, tmp)
		}
		return nil, err
	}
	w := &Writer{
		f:         f,
		tmpPath:   tmp,
		finalPath: path,
		schema:    schema,
		opts:      o,
		colBufs:   make([][]Value, schema.NumCols()),
		stats:     make([]statsBuilder, schema.NumCols()),
		dir:       &directory{Schema: schema},
	}
	hdr := encodeHeader()
	if _, err := f.Write(hdr); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	w.offset = uint64(len(hdr))
	return w, nil
}

// WriteRow appends one row. vals must have exactly one entry per schema
// column, in order. Null values must be created with NullOf; non-null
// values must match the column type.
func (w *Writer) WriteRow(vals ...Value) error {
	if w.closed {
		return ErrWriterClosed
	}
	if w.failed {
		return fmt.Errorf("columnar: writer is in failed state")
	}
	if len(vals) != w.schema.NumCols() {
		return fmt.Errorf("%w: row has %d values, schema has %d columns",
			ErrSchema, len(vals), w.schema.NumCols())
	}
	for i, v := range vals {
		want := w.schema.Columns[i].Type
		if v.Null {
			if v.Kind != want {
				return fmt.Errorf("%w: column %q null value has kind %s, want %s",
					ErrSchema, w.schema.Columns[i].Name, v.Kind, want)
			}
		} else if v.Kind != want {
			return fmt.Errorf("%w: column %q value has kind %s, want %s",
				ErrSchema, w.schema.Columns[i].Name, v.Kind, want)
		}
	}
	for i, v := range vals {
		w.colBufs[i] = append(w.colBufs[i], v)
		w.stats[i].add(v)
	}
	w.pending++
	if w.pending >= w.opts.BlockRows {
		return w.flush()
	}
	return nil
}

// flush encodes and writes the pending block.
func (w *Writer) flush() error {
	if w.pending == 0 {
		return nil
	}
	bm := blockMeta{RowCount: uint32(w.pending)}
	for c := range w.schema.Columns {
		ch, err := w.encodeChunk(c)
		if err != nil {
			w.failed = true
			return err
		}
		bm.Chunks = append(bm.Chunks, *ch)
	}
	w.dir.Blocks = append(w.dir.Blocks, bm)
	w.pending = 0
	for i := range w.colBufs {
		w.colBufs[i] = w.colBufs[i][:0]
		w.stats[i] = statsBuilder{}
	}
	return nil
}

// encodeChunk encodes one column of the pending block and writes it.
func (w *Writer) encodeChunk(col int) (*chunkMeta, error) {
	vals := w.colBufs[col]
	kind := w.schema.Columns[col].Type

	// Null bitmap: bit i set means row i is null.
	bitmapLen := (len(vals) + 7) / 8
	bitmap := make([]byte, bitmapLen)
	nonNull := make([]Value, 0, len(vals))
	for i, v := range vals {
		if v.Null {
			bitmap[i/8] |= 1 << uint(i%8)
		} else {
			nonNull = append(nonNull, v)
		}
	}

	enc, encoded, err := selectEncoding(nonNull, kind)
	if err != nil {
		return nil, err
	}

	// Raw chunk payload: bitmap length, bitmap, encoded values.
	raw := &byteWriter{}
	raw.uvarint(uint64(bitmapLen))
	raw.bytes(bitmap)
	raw.bytes(encoded)

	stored, comp := maybeCompress(raw.buf, w.opts.Compression)

	ch := &chunkMeta{
		Encoding:    enc,
		EncVersion:  encVersion,
		Compression: comp,
		Offset:      w.offset,
		StoredLen:   uint64(len(stored)),
		RawLen:      uint64(len(raw.buf)),
		CRC32:       crc32Of(stored),
		Stats:       w.stats[col].finish(),
	}
	if _, err := w.f.Write(stored); err != nil {
		return nil, err
	}
	w.offset += uint64(len(stored))
	return ch, nil
}

// maybeCompress compresses raw when configured and beneficial. The choice
// depends only on the input bytes, so it is deterministic.
func maybeCompress(raw []byte, comp Compression) ([]byte, Compression) {
	if comp != CompFlate || len(raw) == 0 {
		return raw, CompNone
	}
	var buf bytes.Buffer
	zw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		return raw, CompNone
	}
	if _, err := zw.Write(raw); err != nil {
		zw.Close()
		return raw, CompNone
	}
	if err := zw.Close(); err != nil {
		return raw, CompNone
	}
	if buf.Len() >= len(raw) {
		return raw, CompNone
	}
	return buf.Bytes(), CompFlate
}

// Close flushes pending rows, writes the footer, and atomically moves the
// file to its final path. After Close the Writer cannot be reused.
func (w *Writer) Close() error {
	if w.closed {
		return ErrWriterClosed
	}
	w.closed = true
	if err := w.closeInternal(); err != nil {
		w.f.Close()
		os.Remove(w.tmpPath)
		return err
	}
	return nil
}

func (w *Writer) closeInternal() error {
	if w.failed {
		return fmt.Errorf("columnar: writer is in failed state")
	}
	if err := w.flush(); err != nil {
		return err
	}
	dirBytes := encodeDirectory(w.dir)
	trailer := &byteWriter{}
	trailer.bytes(dirBytes)
	var b [4]byte
	putU32(b[:], uint32(len(dirBytes)))
	trailer.bytes(b[:])
	putU32(b[:], crc32Of(dirBytes))
	trailer.bytes(b[:])
	trailer.bytes(fileMagic)
	if _, err := w.f.Write(trailer.buf); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	// Atomic publish: readers never see a partially written file.
	if err := os.Rename(w.tmpPath, w.finalPath); err != nil {
		return err
	}
	return nil
}

// Abort discards the in-progress file.
func (w *Writer) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.f.Close()
	return os.Remove(w.tmpPath)
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}
