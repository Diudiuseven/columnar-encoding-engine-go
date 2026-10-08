package columnar

// Encoding selection: every applicable encoding is tried on the block's
// non-null values and the smallest payload wins. Ties are broken by a
// fixed priority order, so the choice is fully deterministic for a given
// input. EncPlain always applies and is the guaranteed fallback.

// encPriority lists all encodings in tie-break priority order.
var encPriority = []Encoding{EncArith, EncRLE, EncDict, EncBitpack, EncPlain}

// encApplicable reports whether enc can encode values of the given kind.
func encApplicable(enc Encoding, kind Kind, vals []Value) bool {
	switch enc {
	case EncPlain:
		return true
	case EncRLE, EncDict:
		return true
	case EncBitpack:
		return kind == KindInt64
	case EncArith:
		if kind != KindInt64 {
			return false
		}
		_, _, ok := arithFits(vals)
		return ok
	}
	return false
}

// selectEncoding returns the winning encoding and its payload.
func selectEncoding(vals []Value, kind Kind) (Encoding, []byte, error) {
	var bestEnc Encoding
	var bestPayload []byte
	bestSize := -1
	for _, enc := range encPriority {
		if !encApplicable(enc, kind, vals) {
			continue
		}
		payload, err := encodeValues(enc, vals)
		if err != nil {
			return encInvalid, nil, err
		}
		if bestSize < 0 || len(payload) < bestSize {
			bestEnc, bestPayload, bestSize = enc, payload, len(payload)
		}
	}
	return bestEnc, bestPayload, nil
}
