package columnar

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentReaders hammers one file from many goroutines, both
// through a shared Reader and per-goroutine Readers. Run with -race.
func TestConcurrentReaders(t *testing.T) {
	rows := makeRows(4000)
	path := writeTestFile(t, rows, &Options{BlockRows: 256, Compression: CompFlate})

	shared, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()

	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, readers*2)
	for g := 0; g < readers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			var own *Reader
			if g%2 == 1 {
				var err error
				own, err = Open(path)
				if err != nil {
					errs <- err
					return
				}
				defer own.Close()
			}
			r := shared
			if own != nil {
				r = own
			}
			for b := 0; b < r.NumBlocks(); b++ {
				// Projection: alternate column subsets.
				cols := []int{g % 4, (g + 1) % 4}
				data, err := r.ReadBlock(b, cols)
				if err != nil {
					errs <- err
					return
				}
				n, _ := r.BlockRows(b)
				for i := 0; i < n; i++ {
					rowIdx := b*256 + i
					for k, c := range cols {
						if !valueEq(data[k][i], rows[rowIdx][c]) {
							errs <- errors.New("value mismatch under concurrency")
							return
						}
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestSingleWriterEnforced verifies a second writer on the same path is
// rejected while the first is active.
func TestSingleWriterEnforced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.cen")
	w1, err := NewWriter(path, testSchema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(path, testSchema(), nil); !errors.Is(err, ErrWriterExists) {
		t.Fatalf("second writer: got %v, want ErrWriterExists", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}
	// After close, a new writer may target the path again.
	w2, err := NewWriter(path, testSchema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReaderNeverSeesPartialWrite checks that while a writer is active,
// the final path is absent or a complete previous version, and the temp
// file is not a valid readable file.
func TestReaderNeverSeesPartialWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.cen")

	// Version 1: 10 rows.
	w, err := NewWriter(path, testSchema(), &Options{BlockRows: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := w.WriteRow(Int64(int64(i)), String("v1"), Float64(1), Bool(true)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Start version 2 with 20 rows; write some but do not close.
	w2, err := NewWriter(path, testSchema(), &Options{BlockRows: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := w2.WriteRow(Int64(int64(100+i)), String("v2"), Float64(2), Bool(false)); err != nil {
			t.Fatal(err)
		}
	}
	// Mid-write: readers still see the complete old version.
	r, err := Open(path)
	if err != nil {
		t.Fatalf("open during write: %v", err)
	}
	vals, err := r.ReadBlockColumn(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 10 || vals[0].I64 != 0 {
		t.Fatalf("reader saw partial write: %d rows, first=%v", len(vals), vals[0])
	}
	r.Close()
	// The temp file itself is not a valid complete file.
	if _, err := Open(path + ".tmp"); err == nil {
		t.Fatal("temp file opened as valid file mid-write")
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	// After close: new version visible.
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	vals, err = r2.ReadBlockColumn(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 20 || vals[0].I64 != 100 {
		t.Fatalf("after close: %d rows, first=%v", len(vals), vals[0])
	}
}

// TestWriterAbort ensures Abort removes the temp file and leaves any
// previous version untouched.
func TestWriterAbort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.cen")
	w, err := NewWriter(path, testSchema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteRow(Int64(1), String("a"), Float64(1), Bool(true))
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("final file exists after abort")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file exists after abort")
	}
}

// TestSchemaValidation covers writer-side input validation.
func TestSchemaValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewWriter(filepath.Join(dir, "x.cen"), Schema{}, nil); err == nil {
		t.Fatal("empty schema accepted")
	}
	dup := Schema{Columns: []Column{{Name: "a", Type: KindInt64}, {Name: "a", Type: KindInt64}}}
	if _, err := NewWriter(filepath.Join(dir, "y.cen"), dup, nil); err == nil {
		t.Fatal("duplicate column names accepted")
	}
	w, err := NewWriter(filepath.Join(dir, "z.cen"), testSchema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRow(Int64(1), String("a"), Float64(1)); !errors.Is(err, ErrSchema) {
		t.Fatalf("short row: got %v", err)
	}
	if err := w.WriteRow(Int64(1), Int64(2), Float64(1), Bool(true)); !errors.Is(err, ErrSchema) {
		t.Fatalf("wrong kind: got %v", err)
	}
	if err := w.WriteRow(Int64(1), String("a"), Float64(1), Bool(true)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRow(Int64(1), String("a"), Float64(1), Bool(true)); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("write after close: got %v", err)
	}
}
