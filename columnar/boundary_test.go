package columnar

import (
	"math/rand"
	"path/filepath"
	"testing"
)

// TestEncodingBoundaries exercises values at encoding decision edges.
func TestEncodingBoundaries(t *testing.T) {
	cases := map[string][]int64{
		"two-distinct":     {0, 1, 0, 1, 0, 1, 0, 1},
		"dict-256-limit":   makeSeq(0, 256, 4), // 256 distinct, repeated
		"dict-257-over":    makeSeq(0, 257, 4), // 257 distinct
		"bitpack-48bit":    {0, 1 << 47, 1 << 48, 5, 1<<48 - 1, 1 << 30},
		"bitpack-over-48":  {0, 1 << 49, 3, 1<<50 + 7, 9},
		"delta-neg":        {100, 90, 80, 70, 60, 50},
		"delta-overflow":   {1<<62 - 4, 1 << 62, -(1 << 62), -(1 << 62) + 4},
		"single-value":     {42},
		"two-values-ap":    {7, 9},
		"empty":            {},
		"alternating-runs": {1, 1, 1, 2, 2, 2, 1, 1, 1, 2, 2, 2},
	}
	for name, vals := range cases {
		t.Run(name, func(t *testing.T) {
			id := chooseIntEncoding(vals)
			roundTrip(t, id, vals)
		})
	}
}

func makeSeq(start, distinct, reps int) []int64 {
	var out []int64
	for r := 0; r < reps; r++ {
		for i := 0; i < distinct; i++ {
			out = append(out, int64(start+i))
		}
	}
	return out
}

// TestRandomRoundTrip writes random data of all types and reads it back.
func TestRandomRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	path := filepath.Join(t.TempDir(), "rand.ceng")
	w, err := NewWriter(path, testSchema(), &Config{BlockRows: 128})
	if err != nil {
		t.Fatal(err)
	}
	var want Batch
	want.Cols = make([]ColumnBatch, 3)
	for b := 0; b < 25; b++ {
		n := 1 + rng.Intn(200)
		batch := &Batch{Cols: make([]ColumnBatch, 3)}
		for i := 0; i < n; i++ {
			for ci := 0; ci < 3; ci++ {
				null := rng.Intn(10) == 0
				batch.Cols[ci].Nulls = append(batch.Cols[ci].Nulls, null)
				if null {
					continue
				}
				switch ci {
				case 0:
					var v int64
					switch rng.Intn(4) {
					case 0:
						v = int64(rng.Intn(5)) // low cardinality
					case 1:
						v = int64(i) // arithmetic
					case 2:
						v = int64(rng.Intn(1000)) // narrow
					default:
						v = rng.Int63() // wide
					}
					batch.Cols[0].Ints = append(batch.Cols[0].Ints, v)
				case 1:
					if rng.Intn(2) == 0 {
						batch.Cols[1].Strs = append(batch.Cols[1].Strs, string(rune('a'+rng.Intn(3))))
					} else {
						batch.Cols[1].Strs = append(batch.Cols[1].Strs, string(rune(rng.Intn(0x2FFF))))
					}
				case 2:
					batch.Cols[2].Floats = append(batch.Cols[2].Floats, rng.NormFloat64())
				}
			}
		}
		if err := w.WriteBatch(batch); err != nil {
			t.Fatal(err)
		}
		for ci := 0; ci < 3; ci++ {
			want.Cols[ci].Nulls = append(want.Cols[ci].Nulls, batch.Cols[ci].Nulls...)
			want.Cols[ci].Ints = append(want.Cols[ci].Ints, batch.Cols[ci].Ints...)
			want.Cols[ci].Strs = append(want.Cols[ci].Strs, batch.Cols[ci].Strs...)
			want.Cols[ci].Floats = append(want.Cols[ci].Floats, batch.Cols[ci].Floats...)
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
	s, err := r.Scan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, s)
	assertBatchEqual(t, &want, got)
}

// TestLargeStream writes many rows in small batches to exercise the
// streaming path across block boundaries.
func TestLargeStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.ceng")
	cfg := &Config{BlockRows: 1000}
	w, err := NewWriter(path, testSchema(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	const total = 100_000
	for base := 0; base < total; base += 777 {
		n := 777
		if base+n > total {
			n = total - base
		}
		if err := w.WriteBatch(genBatch(base, n)); err != nil {
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
	if r.NumRows() != total {
		t.Fatalf("NumRows = %d, want %d", r.NumRows(), total)
	}
	// Filtered count: id == r*10 for r=50000 -> exactly one row.
	s, err := r.Scan([]string{"id"}, []Predicate{{Column: "id", Op: Eq, Value: int64(50000 * 10)}})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, s)
	if len(got.Cols[0].Ints) != 1 || got.Cols[0].Ints[0] != 500000 {
		t.Fatalf("Eq lookup got %v", got.Cols[0].Ints)
	}
}
