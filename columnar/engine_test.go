package columnar

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// --- Determinism ---

func TestDeterministicOutput(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.ceng")
	p2 := filepath.Join(dir, "b.ceng")
	cfg := &Config{BlockRows: 33}
	batches := []*Batch{genBatch(0, 100), genBatch(100, 100), genBatch(200, 37)}
	writeTestFile(t, p1, cfg, batches...)
	writeTestFile(t, p2, cfg, batches...)
	b1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("same input and config produced different bytes")
	}
}

// --- Extreme values ---

func TestExtremeValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extreme.ceng")
	ints := []int64{math.MinInt64, math.MaxInt64, 0, -1, 1, math.MinInt64 + 1, math.MaxInt64 - 1}
	floats := []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -0.0, math.SmallestNonzeroFloat64, math.MaxFloat64, -math.MaxFloat64}
	strs := []string{"", "a", string(make([]byte, 10000)), "日本語🍇", "\x00\x01\x02"}
	n := len(ints)
	b := &Batch{Cols: []ColumnBatch{
		{Nulls: make([]bool, n), Ints: ints},
		{Nulls: make([]bool, n), Strs: []string{"", "x", "", "y", "", "z", ""}},
		{Nulls: make([]bool, n), Floats: floats[:n]},
	}}
	// Pad string column values to n non-null entries.
	b.Cols[1].Strs = nil
	for i := 0; i < n; i++ {
		b.Cols[1].Strs = append(b.Cols[1].Strs, strs[i%len(strs)])
	}
	writeTestFile(t, path, &Config{BlockRows: 3}, b)

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
	assertBatchEqual(t, b, got)
}

func TestAllNullColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allnull.ceng")
	n := 50
	b := &Batch{Cols: []ColumnBatch{
		{Nulls: make([]bool, n)}, // all null
		{Nulls: make([]bool, n)},
		{Nulls: make([]bool, n)},
	}}
	for i := 0; i < n; i++ {
		b.Cols[0].Nulls[i] = true
		b.Cols[1].Nulls[i] = i%2 == 0
		if i%2 != 0 {
			b.Cols[1].Strs = append(b.Cols[1].Strs, "v")
		}
		b.Cols[2].Nulls[i] = false
		b.Cols[2].Floats = append(b.Cols[2].Floats, float64(i))
	}
	writeTestFile(t, path, &Config{BlockRows: 16}, b)
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
	assertBatchEqual(t, b, got)
	// IsNull on the all-null column matches every row.
	s2, err := r.Scan([]string{"id"}, []Predicate{{Column: "id", Op: IsNull}})
	if err != nil {
		t.Fatal(err)
	}
	got2 := collect(t, s2)
	if len(got2.Cols[0].Nulls) != n {
		t.Fatalf("IsNull matched %d rows, want %d", len(got2.Cols[0].Nulls), n)
	}
	// Eq on all-null column matches nothing.
	s3, err := r.Scan([]string{"id"}, []Predicate{{Column: "id", Op: Eq, Value: int64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if s3.Next() {
		t.Fatal("Eq on all-null column must match nothing")
	}
}

// --- Corruption ---

func TestCorruptionDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.ceng")
	writeTestFile(t, path, &Config{BlockRows: 32}, genBatch(0, 100))
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Truncations must yield ErrTruncated (or a corruption error for
	// structurally complete-but-wrong prefixes), never a panic.
	for _, cut := range []int64{1, 5, 8, 20, int64(len(orig)) / 2, int64(len(orig)) - 3} {
		p := filepath.Join(dir, "trunc.ceng")
		if err := os.WriteFile(p, orig[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("cut=%d: panic: %v", cut, rec)
				}
			}()
			r, err := Open(p)
			if err != nil {
				if !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrCorrupt) {
					t.Fatalf("cut=%d: unexpected error %v", cut, err)
				}
				return
			}
			defer r.Close()
			s, serr := r.Scan(nil, nil)
			if serr != nil {
				return
			}
			for s.Next() {
			}
			if s.Err() == nil {
				t.Fatalf("cut=%d: truncated file read without error", cut)
			}
		}()
	}

	// Byte flips anywhere must be detected (checksum or structure).
	for _, off := range []int{0, 3, 6, 12, 30, len(orig) - 5, len(orig) - 20, len(orig) - 40} {
		bad := bytes.Clone(orig)
		bad[off] ^= 0xA5
		p := filepath.Join(dir, "flip.ceng")
		if err := os.WriteFile(p, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("off=%d: panic: %v", off, rec)
				}
			}()
			r, err := Open(p)
			if err != nil {
				return
			}
			defer r.Close()
			s, serr := r.Scan(nil, nil)
			if serr != nil {
				return
			}
			for s.Next() {
			}
			if s.Err() == nil {
				t.Fatalf("off=%d: tampered file read without error", off)
			}
		}()
	}
}

// --- Versioning ---

func TestUnsupportedFormatVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.ceng")
	writeTestFile(t, path, nil, genBatch(0, 10))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Bump the format version beyond what we support.
	binary.LittleEndian.PutUint16(raw[4:6], maxSupportedVersion+1)
	p2 := filepath.Join(dir, "v2.ceng")
	if err := os.WriteFile(p2, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Open(p2)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v, want ErrUnsupportedVersion", err)
	}
}

// patchFirstChunkEncoding rewrites the encoding id/version of the first
// chunk of the first block and fixes the header CRC.
func patchFirstChunkEncoding(t *testing.T, raw []byte, encID, encVer byte) []byte {
	t.Helper()
	raw = bytes.Clone(raw)
	hdrOff := int64(headerLen)
	ncol := int(binary.LittleEndian.Uint16(raw[hdrOff+4:]))
	raw[hdrOff+6] = encID
	raw[hdrOff+7] = encVer
	hdrLen := blockHeaderLen(ncol)
	binary.LittleEndian.PutUint32(raw[hdrOff+int64(hdrLen)-4:], crc32.Checksum(raw[hdrOff:hdrOff+int64(hdrLen)-4], crcTable))
	return raw
}

func TestUnknownEncodingAndVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.ceng")
	writeTestFile(t, path, nil, genBatch(0, 10))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Unknown encoding id.
	bad := patchFirstChunkEncoding(t, raw, 200, encVersion)
	p := filepath.Join(dir, "unk.ceng")
	os.WriteFile(p, bad, 0o644)
	r, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.Scan(nil, nil)
	for s.Next() {
	}
	if !errors.Is(s.Err(), ErrUnknownEncoding) {
		t.Fatalf("got %v, want ErrUnknownEncoding", s.Err())
	}
	r.Close()

	// Future encoding version.
	bad = patchFirstChunkEncoding(t, raw, byte(EncPlain), encVersion+1)
	p = filepath.Join(dir, "newver.ceng")
	os.WriteFile(p, bad, 0o644)
	r, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s, _ = r.Scan(nil, nil)
	for s.Next() {
	}
	if !errors.Is(s.Err(), ErrUnsupportedVersion) {
		t.Fatalf("got %v, want ErrUnsupportedVersion", s.Err())
	}
	r.Close()
}

// --- Concurrency & single writer ---

func TestConcurrentReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.ceng")
	var batches []*Batch
	for i := 0; i < 20; i++ {
		batches = append(batches, genBatch(i*50, 50))
	}
	writeTestFile(t, path, &Config{BlockRows: 64}, batches...)

	const readers = 8
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for k := 0; k < readers; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			r, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			defer r.Close()
			var preds []Predicate
			var proj []string
			switch k % 3 {
			case 0:
				preds = []Predicate{{Column: "id", Op: Ge, Value: int64(500)}}
				proj = []string{"name"}
			case 1:
				preds = []Predicate{{Column: "score", Op: Lt, Value: 200.0}}
			case 2:
				preds = []Predicate{{Column: "name", Op: IsNull}}
				proj = []string{"id", "score"}
			}
			s, err := r.Scan(proj, preds)
			if err != nil {
				errs <- err
				return
			}
			total := 0
			for s.Next() {
				total += s.Batch().Rows()
			}
			if s.Err() != nil {
				errs <- s.Err()
				return
			}
			if total == 0 {
				errs <- errors.New("reader got no rows")
			}
		}(k)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestSingleWriterAndAtomicPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.ceng")
	w, err := NewWriter(path, testSchema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A second writer on the same path must fail.
	if _, err := NewWriter(path, testSchema(), nil); !errors.Is(err, ErrWriterExists) {
		t.Fatalf("second writer: got %v, want ErrWriterExists", err)
	}
	if err := w.WriteBatch(genBatch(0, 10)); err != nil {
		t.Fatal(err)
	}
	// While the writer is active, the final path must not exist, so
	// readers can never observe a half-written file.
	if _, err := Open(path); !os.IsNotExist(err) {
		t.Fatalf("open during write: got %v, want not-exist", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// After Close the file is complete and readable.
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.NumRows() != 10 {
		t.Fatalf("NumRows = %d, want 10", r.NumRows())
	}
	r.Close()
	// Writing after close must fail.
	if err := w.WriteBatch(genBatch(0, 1)); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("write after close: got %v", err)
	}
}

func TestNaNPredicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nan.ceng")
	b := &Batch{Cols: []ColumnBatch{
		{Nulls: []bool{false, false, false}, Ints: []int64{1, 2, 3}},
		{Nulls: []bool{false, false, false}, Strs: []string{"a", "b", "c"}},
		{Nulls: []bool{false, false, false}, Floats: []float64{math.NaN(), 1.5, 2.5}},
	}}
	writeTestFile(t, path, nil, b)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Eq NaN matches exactly the NaN row.
	s, err := r.Scan([]string{"id"}, []Predicate{{Column: "score", Op: Eq, Value: math.NaN()}})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, s)
	if len(got.Cols[0].Ints) != 1 || got.Cols[0].Ints[0] != 1 {
		t.Fatalf("Eq NaN matched %v, want [1]", got.Cols[0].Ints)
	}
	// Gt 1.0 must skip the NaN row.
	s2, err := r.Scan([]string{"id"}, []Predicate{{Column: "score", Op: Gt, Value: 1.0}})
	if err != nil {
		t.Fatal(err)
	}
	got2 := collect(t, s2)
	if len(got2.Cols[0].Ints) != 2 || got2.Cols[0].Ints[0] != 2 || got2.Cols[0].Ints[1] != 3 {
		t.Fatalf("Gt 1.0 matched %v, want [2 3]", got2.Cols[0].Ints)
	}
}
