// Package columnar implements a columnar encoding and compression engine.
//
// Data is written in blocks; each column within a block is encoded
// independently with an automatically chosen encoding. The on-disk
// format is versioned, deterministic and safe for concurrent readers.
package columnar

import "errors"

// Type is the logical data type of a column.
type Type int

const (
	Int64Type   Type = 1
	Float64Type Type = 2
	StringType  Type = 3
)

func (t Type) String() string {
	switch t {
	case Int64Type:
		return "int64"
	case Float64Type:
		return "float64"
	case StringType:
		return "string"
	default:
		return "unknown"
	}
}

// Column describes one column of the schema.
type Column struct {
	Name string
	Type Type
}

// Schema is the ordered list of columns of a file.
type Schema struct {
	Columns []Column
}

// ColumnIndex returns the index of the named column, or -1.
func (s *Schema) ColumnIndex(name string) int {
	for i, c := range s.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// ColumnBatch holds the data of one column for a set of rows.
// Exactly one of Ints/Floats/Strs is used, matching the column type.
// Nulls marks null positions; null entries do not appear in the value
// slices (value slices contain only non-null values, in row order).
type ColumnBatch struct {
	Nulls  []bool
	Ints   []int64
	Floats []float64
	Strs   []string
}

// Rows returns the number of rows in the batch.
func (c *ColumnBatch) Rows() int { return len(c.Nulls) }

// Batch is a set of column batches with an equal row count.
type Batch struct {
	Cols []ColumnBatch
}

// Rows returns the row count of the batch (0 for an empty batch).
func (b *Batch) Rows() int {
	if len(b.Cols) == 0 {
		return 0
	}
	return b.Cols[0].Rows()
}

// Sentinel errors. Use errors.Is to test for them.
var (
	// ErrCorrupt indicates the input failed integrity checks (bad magic,
	// bad checksum, malformed structure).
	ErrCorrupt = errors.New("columnar: corrupt data")
	// ErrTruncated indicates the input ends before a complete structure
	// could be read.
	ErrTruncated = errors.New("columnar: truncated data")
	// ErrUnsupportedVersion indicates a format or encoding version newer
	// than this library understands.
	ErrUnsupportedVersion = errors.New("columnar: unsupported version")
	// ErrUnknownEncoding indicates an encoding identifier this library
	// does not know.
	ErrUnknownEncoding = errors.New("columnar: unknown encoding")
	// ErrWriterClosed is returned when writing after Close.
	ErrWriterClosed = errors.New("columnar: writer already closed")
	// ErrSchema is returned for schema/data mismatches.
	ErrSchema = errors.New("columnar: schema mismatch")
	// ErrWriterExists indicates another writer holds the file.
	ErrWriterExists = errors.New("columnar: another writer is active")
)

// Op is a comparison operator used in predicates.
type Op int

const (
	Eq Op = iota + 1
	Ne
	Lt
	Le
	Gt
	Ge
	IsNull
	NotNull
)

// Predicate is a single-column filter condition. Value must match the
// column type (int64, float64 or string) and is ignored for
// IsNull/NotNull.
type Predicate struct {
	Column string
	Op     Op
	Value  any
}
