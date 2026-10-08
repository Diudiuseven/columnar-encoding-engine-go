package columnar

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// writeOneColumn writes a single-column file and returns the encoding
// chosen for its first block.
func chosenEncoding(t *testing.T, kind Kind, vals []Value) Encoding {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sel.cen")
	schema := Schema{Columns: []Column{{Name: "c", Type: kind}}}
	w, err := NewWriter(path, schema, &Options{BlockRows: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vals {
		if err := w.WriteRow(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.NumBlocks() != 1 {
		t.Fatalf("got %d blocks", r.NumBlocks())
	}
	return r.dir.Blocks[0].Chunks[0].Encoding
}

func TestEncodingSelection(t *testing.T) {
	// Arithmetic sequence -> arith.
	arith := make([]Value, 100)
	for i := range arith {
		arith[i] = Int64(int64(7 + 3*i))
	}
	if got := chosenEncoding(t, KindInt64, arith); got != EncArith {
		t.Fatalf("arith sequence: got %s", got)
	}

	// Long runs -> rle.
	runs := make([]Value, 0, 200)
	for i := 0; i < 20; i++ {
		for j := 0; j < 10; j++ {
			runs = append(runs, Int64(int64(i*i+1))) // non-arithmetic run values
		}
	}
	if got := chosenEncoding(t, KindInt64, runs); got != EncRLE {
		t.Fatalf("runs: got %s", got)
	}

	// Low-cardinality strings -> dict.
	dict := make([]Value, 200)
	for i := range dict {
		dict[i] = String([]string{"alpha", "beta", "gamma"}[i%3])
	}
	if got := chosenEncoding(t, KindString, dict); got != EncDict {
		t.Fatalf("low-cardinality strings: got %s", got)
	}

	// Small-range shuffled ints -> bitpack.
	bp := make([]Value, 100)
	for i := range bp {
		bp[i] = Int64(int64((i*37 + 11) % 16))
	}
	if got := chosenEncoding(t, KindInt64, bp); got != EncBitpack {
		t.Fatalf("small-range ints: got %s", got)
	}

	// High-entropy strings -> plain fallback.
	pl := make([]Value, 64)
	for i := range pl {
		pl[i] = String(string([]byte{byte(i), byte(i*3 + 1), byte(i*7 + 2), byte(i*13 + 5)}))
	}
	if got := chosenEncoding(t, KindString, pl); got != EncPlain {
		t.Fatalf("high-entropy strings: got %s", got)
	}
}

// TestGoldenFormat pins the exact bytes of a tiny file so any accidental
// format change is caught, and proves old (v1) files stay readable: the
// golden bytes below were produced by format version 1 and must always
// decode to the same values.
func TestGoldenFormat(t *testing.T) {
	schema := Schema{Columns: []Column{
		{Name: "n", Type: KindInt64},
		{Name: "s", Type: KindString},
	}}
	rows := [][]Value{
		{Int64(1), String("a")},
		{Int64(2), NullOf(KindString)},
		{Int64(3), String("c")},
	}
	path := filepath.Join(t.TempDir(), "golden.cen")
	w, err := NewWriter(path, schema, &Options{BlockRows: 16})
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
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if goldenHex != "" {
		want, err := hex.DecodeString(goldenHex)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("format drift:\n got %x\nwant %x", got, want)
		}
	} else {
		t.Logf("golden bytes: %x", got)
	}

	// The golden file (written by v1) must decode to the original rows.
	golden, err := hex.DecodeString(goldenHex)
	if err != nil {
		t.Skip("golden not recorded yet")
	}
	gp := filepath.Join(t.TempDir(), "golden-read.cen")
	if err := os.WriteFile(gp, golden, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(gp)
	if err != nil {
		t.Fatalf("v1 golden file not readable: %v", err)
	}
	defer r.Close()
	for c := 0; c < 2; c++ {
		vals, err := r.ReadBlockColumn(0, c)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			if !valueEq(v, rows[i][c]) {
				t.Fatalf("golden col %d row %d: got %+v want %+v", c, i, v, rows[i][c])
			}
		}
	}
}

// goldenHex is the byte-exact v1 encoding of the TestGoldenFormat file.
const goldenHex = "43454e520100000001000100000000000000022401020161016302016e010173030103040100080c0c6d67870103000101000000000000000300000000000000010100140606b7e6573c0301010161016337000000e0fcdc5e43454e52"
