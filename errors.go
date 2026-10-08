package columnar

import "errors"

// Sentinel errors. Use errors.Is to match them; they may be wrapped with
// additional context.
var (
	// ErrUnsupportedVersion is returned when the file format version or an
	// encoding version is newer than this library supports.
	ErrUnsupportedVersion = errors.New("columnar: unsupported version")

	// ErrUnsupportedEncoding is returned when a chunk uses an encoding id
	// this library does not know.
	ErrUnsupportedEncoding = errors.New("columnar: unsupported encoding")

	// ErrCorrupt is returned when the input bytes fail integrity checks
	// (bad magic, CRC mismatch, truncation, inconsistent lengths).
	ErrCorrupt = errors.New("columnar: corrupt data")

	// ErrSchema is returned for schema mismatches between writer input
	// and the declared schema.
	ErrSchema = errors.New("columnar: schema violation")

	// ErrWriterClosed is returned when writing to a closed writer.
	ErrWriterClosed = errors.New("columnar: writer already closed")

	// ErrWriterExists is returned when another writer holds the target.
	ErrWriterExists = errors.New("columnar: another writer is active")
)
