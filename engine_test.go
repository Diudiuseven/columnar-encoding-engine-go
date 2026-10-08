package columnar

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func testSchema() Schema {
	return Schema{Columns: []Column{
		{Name: "id", Type: KindInt64},
		{Name: "name", Type: KindString},
		{Name: "score", Type: KindFloat64},
		{Name: "active", Type: KindBool},
	}}
}

// writeTestFile writes rows and returns the file path.
func writeTestFile(t *testing.T, rows [][]Value, opts *Options) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.cen")
	w, err := NewWriter(path, testSchema(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := w.WriteRow(row...); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func makeRows(n int) [][]Value {
	rows := make([][]Value, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("user-%d", i%10)
		if i%7 == 0 {
			rows[i] = []Value{Int64(int64(i)), NullOf(KindString), Float64(float64(i) * 1.5), Bool(i%2 == 0)}
		} else if i%11 == 0 {
			rows[i] = []Value{Int64(int64(i)), String(name), NullOf(KindFloat64), Bool(i%2 == 0)}
		} else {
			rows[i] = []Value{Int64(int64(i)), String(name), Float64(float64(i) * 1.5), Bool(i%2 == 0)}
		}
	}
	return rows
}

func TestWriteReadRoundTrip(t *testing.T) {
	rows := makeRows(20000) // spans multiple blocks at small BlockRows
	path := writeTestFile(t, rows, &Options{BlockRows: 1000})

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if r.NumBlocks() != 20 {
		t.Fatalf("got %d blocks, want 20", r.NumBlocks())
	}
	got := make([][]Value, 0, len(rows))
	ncols := r.Schema().NumCols()
	for b := 0; b < r.NumBlocks(); b++ {
		cols := make([]int, ncols)
		for i := range cols {
			cols[i] = i
		}
		data, err := r.ReadBlock(b, cols)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := r.BlockRows(b)
		for i := 0; i < n; i++ {
			row := make([]Value, ncols)
			for c := range row {
				row[c] = data[c][i]
			}
			got = append(got, row)
		}
	}
	if len(got) != len(rows) {
		t.Fatalf("got %d rows, want %d", len(got), len(rows))
	}
	for i := range rows {
		for c := range rows[i] {
			if !valueEq(got[i][c], rows[i][c]) {
				t.Fatalf("row %d col %d: got %+v want %+v", i, c, got[i][c], rows[i][c])
			}
		}
	}
}

func TestEmptyFile(t *testing.T) {
	path := writeTestFile(t, nil, nil)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.NumBlocks() != 0 {
		t.Fatalf("got %d blocks, want 0", r.NumBlocks())
	}
}

func TestAllNullColumn(t *testing.T) {
	rows := [][]Value{
		{Int64(1), NullOf(KindString), Float64(1), Bool(true)},
		{Int64(2), NullOf(KindString), Float64(2), Bool(false)},
	}
	path := writeTestFile(t, rows, &Options{BlockRows: 100})
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	st, err := r.Stats(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if st.NullCount != 2 || st.RowCount != 2 || st.HasMinMax {
		t.Fatalf("bad stats: %+v", st)
	}
	// IS NULL must match; equality must not.
	blocks, err := r.MayMatch(Predicate{Column: "name", Op: OpIsNull})
	if err != nil || len(blocks) != 1 {
		t.Fatalf("IS NULL: blocks=%v err=%v", blocks, err)
	}
	blocks, err = r.MayMatch(Predicate{Column: "name", Op: OpEq, Val: String("x")})
	if err != nil || len(blocks) != 0 {
		t.Fatalf("Eq on all-null: blocks=%v err=%v", blocks, err)
	}
	vals, err := r.ReadBlockColumn(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vals {
		if !v.Null {
			t.Fatal("expected null")
		}
	}
}

func TestPredicateSkipCorrectness(t *testing.T) {
	// Block 0: ids 0..999, block 1: ids 1000..1999, ...
	rows := make([][]Value, 0, 3000)
	for i := 0; i < 3000; i++ {
		rows = append(rows, []Value{Int64(int64(i)), String("n"), Float64(0), Bool(false)})
	}
	path := writeTestFile(t, rows, &Options{BlockRows: 1000})
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	blocks, err := r.MayMatch(Predicate{Column: "id", Op: OpEq, Val: Int64(1500)})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0] != 1 {
		t.Fatalf("Eq 1500: got blocks %v, want [1]", blocks)
	}

	blocks, err = r.MayMatch(Predicate{Column: "id", Op: OpGe, Val: Int64(2000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0] != 2 {
		t.Fatalf("Ge 2000: got blocks %v, want [2]", blocks)
	}

	blocks, err = r.MayMatch(Predicate{Column: "id", Op: OpLt, Val: Int64(0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Fatalf("Lt 0: got blocks %v, want none", blocks)
	}

	// Verify against brute force: every skipped block must truly have no
	// matching row.
	preds := []Predicate{
		{Column: "id", Op: OpEq, Val: Int64(1234)},
		{Column: "id", Op: OpGt, Val: Int64(2500)},
		{Column: "id", Op: OpLe, Val: Int64(500)},
	}
	for _, p := range preds {
		keep, err := r.MayMatch(p)
		if err != nil {
			t.Fatal(err)
		}
		kept := map[int]bool{}
		for _, b := range keep {
			kept[b] = true
		}
		for b := 0; b < r.NumBlocks(); b++ {
			if kept[b] {
				continue
			}
			vals, err := r.ReadBlockColumn(b, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if matchRow(p, v) {
					t.Fatalf("block %d skipped by %v but contains matching value %v", b, p, v)
				}
			}
		}
	}
}

func matchRow(p Predicate, v Value) bool {
	switch p.Op {
	case OpIsNull:
		return v.Null
	case OpNotNull:
		return !v.Null
	}
	if v.Null {
		return false
	}
	c := compareValues(v, p.Val)
	switch p.Op {
	case OpEq:
		return c == 0
	case OpLt:
		return c < 0
	case OpLe:
		return c <= 0
	case OpGt:
		return c > 0
	case OpGe:
		return c >= 0
	}
	return false
}

func TestProjection(t *testing.T) {
	rows := makeRows(100)
	path := writeTestFile(t, rows, &Options{BlockRows: 64})
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Read only column 1 (name) of block 1.
	vals, err := r.ReadBlock(1, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || len(vals[0]) != 36 {
		t.Fatalf("unexpected projection shape: %d cols", len(vals))
	}
	for i, v := range vals[0] {
		if !valueEq(v, rows[64+i][1]) {
			t.Fatalf("row %d: got %+v want %+v", i, v, rows[64+i][1])
		}
	}
}

func TestDeterministicOutput(t *testing.T) {
	rows := makeRows(5000)
	p1 := writeTestFile(t, rows, &Options{BlockRows: 512, Compression: CompFlate})
	p2 := writeTestFile(t, rows, &Options{BlockRows: 512, Compression: CompFlate})
	b1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("same input produced different bytes")
	}
}

func TestCompressionRoundTrip(t *testing.T) {
	rows := makeRows(5000)
	path := writeTestFile(t, rows, &Options{BlockRows: 512, Compression: CompFlate})
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	vals, err := r.ReadBlockColumn(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vals {
		if !valueEq(v, rows[i][0]) {
			t.Fatalf("row %d mismatch", i)
		}
	}
}

func TestExtremeValues(t *testing.T) {
	rows := [][]Value{
		{Int64(math.MinInt64), String(""), Float64(math.NaN()), Bool(false)},
		{Int64(math.MaxInt64), String("☃"), Float64(math.Inf(-1)), Bool(true)},
		{NullOf(KindInt64), NullOf(KindString), NullOf(KindFloat64), NullOf(KindBool)},
		{Int64(0), String("x"), Float64(math.SmallestNonzeroFloat64), Bool(false)},
	}
	path := writeTestFile(t, rows, &Options{BlockRows: 2})
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.NumBlocks() != 2 {
		t.Fatalf("got %d blocks", r.NumBlocks())
	}
	for b := 0; b < 2; b++ {
		for c := 0; c < 4; c++ {
			vals, err := r.ReadBlockColumn(b, c)
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if !valueEq(v, rows[b*2+i][c]) {
					t.Fatalf("block %d col %d row %d: got %+v want %+v", b, c, i, v, rows[b*2+i][c])
				}
			}
		}
	}
}
