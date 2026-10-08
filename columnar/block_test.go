package columnar

import (
	"bytes"
	"testing"
)

func testSchema() *Schema {
	return &Schema{Columns: []Column{
		{Name: "id", Type: Int64Type},
		{Name: "name", Type: StringType},
		{Name: "score", Type: Float64Type},
	}}
}

func testBatch() []ColumnBatch {
	return []ColumnBatch{
		{Nulls: []bool{false, false, true, false}, Ints: []int64{1, 2, 4}},
		{Nulls: []bool{false, true, false, false}, Strs: []string{"a", "b", "a"}},
		{Nulls: []bool{false, false, false, true}, Floats: []float64{1.5, -2.25, 3.75}},
	}
}

func TestBlockRoundTrip(t *testing.T) {
	schema := testSchema()
	cols := testBatch()
	data, stats, err := encodeBlock(schema, cols, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 3 {
		t.Fatalf("got %d stats", len(stats))
	}
	if stats[0].NullCount != 1 || stats[1].NullCount != 1 || stats[2].NullCount != 1 {
		t.Fatalf("bad null counts: %+v", stats)
	}
	h, err := parseBlockHeader(data[:blockHeaderLen(3)], 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.numRows != 4 || len(h.chunks) != 3 {
		t.Fatalf("bad header: %+v", h)
	}
	for i, col := range schema.Columns {
		m := h.chunks[i]
		cb, err := decodeChunk(col.Type, m, data[m.Offset:m.Offset+m.Length+4], 4)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		orig := cols[i]
		for r := 0; r < 4; r++ {
			if cb.Nulls[r] != orig.Nulls[r] {
				t.Fatalf("col %d row %d: null mismatch", i, r)
			}
		}
		switch col.Type {
		case Int64Type:
			for j, v := range orig.Ints {
				if cb.Ints[j] != v {
					t.Fatalf("col %d val %d: got %d want %d", i, j, cb.Ints[j], v)
				}
			}
		case StringType:
			for j, v := range orig.Strs {
				if cb.Strs[j] != v {
					t.Fatalf("col %d val %d: got %q want %q", i, j, cb.Strs[j], v)
				}
			}
		case Float64Type:
			for j, v := range orig.Floats {
				if cb.Floats[j] != v {
					t.Fatalf("col %d val %d: got %v want %v", i, j, cb.Floats[j], v)
				}
			}
		}
	}
}

func TestBlockCorruption(t *testing.T) {
	schema := testSchema()
	data, _, err := encodeBlock(schema, testBatch(), 4)
	if err != nil {
		t.Fatal(err)
	}
	hdrLen := blockHeaderLen(3)
	// Flip a byte in the header: header CRC must catch it.
	bad := bytes.Clone(data[:hdrLen])
	bad[hdrLen/2] ^= 0xff
	if _, err := parseBlockHeader(bad, 0); err == nil {
		t.Fatal("expected header corruption error")
	}
	// Truncate the header.
	if _, err := parseBlockHeader(data[:hdrLen-2], 0); err == nil {
		t.Fatal("expected truncation error")
	}
	// Flip a byte inside a chunk payload: chunk CRC must catch it.
	h, err := parseBlockHeader(data[:hdrLen], 0)
	if err != nil {
		t.Fatal(err)
	}
	m := h.chunks[0]
	badChunk := bytes.Clone(data[m.Offset : m.Offset+m.Length+4])
	badChunk[2] ^= 0xff
	if _, err := decodeChunk(Int64Type, m, badChunk, 4); err == nil {
		t.Fatal("expected chunk corruption error")
	}
}
