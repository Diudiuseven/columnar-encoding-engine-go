package columnar

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
)

// genBatch builds a deterministic batch of n rows starting at row `base`.
func genBatch(base, n int) *Batch {
	ids := &ColumnBatch{}
	names := &ColumnBatch{}
	scores := &ColumnBatch{}
	for i := 0; i < n; i++ {
		r := base + i
		ids.Nulls = append(ids.Nulls, r%7 == 3)
		if r%7 != 3 {
			ids.Ints = append(ids.Ints, int64(r*10))
		}
		names.Nulls = append(names.Nulls, r%5 == 2)
		if r%5 != 2 {
			names.Strs = append(names.Strs, fmt.Sprintf("name-%d", r%20))
		}
		scores.Nulls = append(scores.Nulls, r%11 == 5)
		if r%11 != 5 {
			scores.Floats = append(scores.Floats, float64(r)*1.5)
		}
	}
	return &Batch{Cols: []ColumnBatch{*ids, *names, *scores}}
}

func writeTestFile(t *testing.T, path string, cfg *Config, batches ...*Batch) {
	t.Helper()
	w, err := NewWriter(path, testSchema(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range batches {
		if err := w.WriteBatch(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func collect(t *testing.T, s *Scanner) *Batch {
	t.Helper()
	var out Batch
	ncol := 0
	for s.Next() {
		b := s.Batch()
		if ncol == 0 {
			ncol = len(b.Cols)
			out.Cols = make([]ColumnBatch, ncol)
		}
		for ci := range b.Cols {
			dst := &out.Cols[ci]
			src := &b.Cols[ci]
			dst.Nulls = append(dst.Nulls, src.Nulls...)
			dst.Ints = append(dst.Ints, src.Ints...)
			dst.Floats = append(dst.Floats, src.Floats...)
			dst.Strs = append(dst.Strs, src.Strs...)
		}
	}
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
	return &out
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.ceng")
	cfg := &Config{BlockRows: 100}
	var batches []*Batch
	for i := 0; i < 10; i++ {
		batches = append(batches, genBatch(i*50, 50))
	}
	writeTestFile(t, path, cfg, batches...)

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.NumRows() != 500 {
		t.Fatalf("NumRows = %d, want 500", r.NumRows())
	}
	s, err := r.Scan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, s)
	// Compare against the concatenation of inputs.
	want := &Batch{Cols: make([]ColumnBatch, 3)}
	for _, b := range batches {
		for ci := range b.Cols {
			want.Cols[ci].Nulls = append(want.Cols[ci].Nulls, b.Cols[ci].Nulls...)
			want.Cols[ci].Ints = append(want.Cols[ci].Ints, b.Cols[ci].Ints...)
			want.Cols[ci].Floats = append(want.Cols[ci].Floats, b.Cols[ci].Floats...)
			want.Cols[ci].Strs = append(want.Cols[ci].Strs, b.Cols[ci].Strs...)
		}
	}
	assertBatchEqual(t, want, got)
}

func assertBatchEqual(t *testing.T, want, got *Batch) {
	t.Helper()
	if len(want.Cols) != len(got.Cols) {
		t.Fatalf("column count %d != %d", len(want.Cols), len(got.Cols))
	}
	for ci := range want.Cols {
		w, g := &want.Cols[ci], &got.Cols[ci]
		if len(w.Nulls) != len(g.Nulls) {
			t.Fatalf("col %d: rows %d != %d", ci, len(w.Nulls), len(g.Nulls))
		}
		for i := range w.Nulls {
			if w.Nulls[i] != g.Nulls[i] {
				t.Fatalf("col %d row %d: null %v != %v", ci, i, w.Nulls[i], g.Nulls[i])
			}
		}
		if len(w.Ints) != len(g.Ints) || len(w.Floats) != len(g.Floats) || len(w.Strs) != len(g.Strs) {
			t.Fatalf("col %d: value count mismatch", ci)
		}
		for i := range w.Ints {
			if w.Ints[i] != g.Ints[i] {
				t.Fatalf("col %d val %d: %d != %d", ci, i, w.Ints[i], g.Ints[i])
			}
		}
		for i := range w.Floats {
			if w.Floats[i] != g.Floats[i] && !(math.IsNaN(w.Floats[i]) && math.IsNaN(g.Floats[i])) {
				t.Fatalf("col %d val %d: %v != %v", ci, i, w.Floats[i], g.Floats[i])
			}
		}
		for i := range w.Strs {
			if w.Strs[i] != g.Strs[i] {
				t.Fatalf("col %d val %d: %q != %q", ci, i, w.Strs[i], g.Strs[i])
			}
		}
	}
}

func TestProjectionAndPredicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.ceng")
	cfg := &Config{BlockRows: 64}
	var batches []*Batch
	for i := 0; i < 8; i++ {
		batches = append(batches, genBatch(i*40, 40))
	}
	writeTestFile(t, path, cfg, batches...)

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Project only "name", filter id >= 1000 (row >= 100).
	s, err := r.Scan([]string{"name"}, []Predicate{
		{Column: "id", Op: Ge, Value: int64(1000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, s)
	if len(got.Cols) != 1 {
		t.Fatalf("projection returned %d columns", len(got.Cols))
	}
	// Verify against input rows 100..319, skipping rows where id is
	// null (r%7==3): those cannot match id>=1000.
	wantRows := 0
	row := 0
	for rnum := 100; rnum < 320; rnum++ {
		if rnum%7 == 3 {
			continue
		}
		wantRows++
		wantNull := rnum%5 == 2
		if got.Cols[0].Nulls[row] != wantNull {
			t.Fatalf("row %d (r=%d): null=%v want %v", row, rnum, got.Cols[0].Nulls[row], wantNull)
		}
		if !wantNull {
			wantName := fmt.Sprintf("name-%d", rnum%20)
			// find value index: count non-nulls up to row
			nn := 0
			for i := 0; i < row; i++ {
				if !got.Cols[0].Nulls[i] {
					nn++
				}
			}
			if got.Cols[0].Strs[nn] != wantName {
				t.Fatalf("row %d (r=%d): name=%q want %q", row, rnum, got.Cols[0].Strs[nn], wantName)
			}
		}
		row++
	}
	if row != wantRows {
		t.Fatalf("got %d rows, want %d", row, wantRows)
	}
}

func TestPredicateSkipsBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.ceng")
	cfg := &Config{BlockRows: 50}
	var batches []*Batch
	for i := 0; i < 10; i++ {
		batches = append(batches, genBatch(i*50, 50))
	}
	writeTestFile(t, path, cfg, batches...)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// id values are r*10; max is 4990. A predicate beyond max must match
	// zero blocks.
	s, err := r.Scan(nil, []Predicate{{Column: "id", Op: Gt, Value: int64(10_000_000)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.blocks) != 0 {
		t.Fatalf("expected all blocks pruned, %d remain", len(s.blocks))
	}
	if s.Next() {
		t.Fatal("expected no batches")
	}
	// IsNull on a column with nulls must keep blocks.
	s2, err := r.Scan([]string{"id"}, []Predicate{{Column: "id", Op: IsNull}})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, s2)
	for i, n := range got.Cols[0].Nulls {
		if !n {
			t.Fatalf("row %d: IsNull returned non-null row", i)
		}
	}
	if len(got.Cols[0].Nulls) == 0 {
		t.Fatal("IsNull returned no rows")
	}
}
