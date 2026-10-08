package columnar

// EncodingID identifies a column-chunk encoding on disk.
type EncodingID byte

const (
	EncPlain   EncodingID = 1 // plain: fixed-width / length-prefixed values
	EncBitPack EncodingID = 2 // bit-packed integers with min offset
	EncRLE     EncodingID = 3 // run-length encoding
	EncDict    EncodingID = 4 // dictionary encoding
	EncDelta   EncodingID = 5 // arithmetic-progression (delta) encoding
)

// encVersion is the current version of every encoding. Each chunk stores
// the version of the encoding that produced it, so future revisions can
// coexist with old data.
const encVersion byte = 1
