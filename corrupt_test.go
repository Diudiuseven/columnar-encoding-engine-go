package columnar

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeSmallFile(t *testing.T) string {
	t.Helper()
	rows := makeRows(300)
	return writeTestFile(t, rows, &Options{BlockRows: 100})
}

func TestTruncatedFile(t *testing.T) {
	path := writeSmallFile(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 3, headerSize, len(data) / 2, len(data) - 2} {
		p := filepath.Join(t.TempDir(), "trunc.cen")
		if err := os.WriteFile(p, data[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := Open(p)
		if err == nil {
			r.Close()
			t.Fatalf("truncated to %d bytes: Open succeeded", n)
		}
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("truncated to %d: got %v, want ErrCorrupt", n, err)
		}
	}
}

func TestTamperedChunkByte(t *testing.T) {
	path := writeSmallFile(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the first chunk (right after the header).
	data[headerSize+2] ^= 0xff
	p := filepath.Join(t.TempDir(), "tampered.cen")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(p)
	if err != nil {
		// Also acceptable: corruption detected at open.
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("got %v, want ErrCorrupt", err)
		}
		return
	}
	defer r.Close()
	_, err = r.ReadBlockColumn(0, 0)
	if err == nil {
		t.Fatal("tampered chunk decoded without error")
	}
	if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnsupportedEncoding) && !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v, want a detectable error", err)
	}
}

func TestTamperedDirectory(t *testing.T) {
	path := writeSmallFile(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the directory (before the trailer).
	data[len(data)-trailerSize-3] ^= 0x01
	p := filepath.Join(t.TempDir(), "tampered-dir.cen")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestBadMagic(t *testing.T) {
	path := writeSmallFile(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	p := filepath.Join(t.TempDir(), "badmagic.cen")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestFutureFormatVersion(t *testing.T) {
	path := writeSmallFile(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[4] = formatMajor + 1
	p := filepath.Join(t.TempDir(), "future.cen")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v, want ErrUnsupportedVersion", err)
	}
}

// buildSyntheticFile assembles a file from parts, bypassing the writer,
// to exercise reader handling of unknown encodings and versions.
func buildSyntheticFile(t *testing.T, mutate func(*directory)) string {
	t.Helper()
	schema := testSchema()
	// One block, one row; encode chunk for column 0 only and reuse its
	// bytes for all columns by pointing offsets at it (only col 0 read).
	vals := []Value{Int64(42)}
	enc, payload, err := selectEncoding(vals, KindInt64)
	if err != nil {
		t.Fatal(err)
	}
	raw := &byteWriter{}
	raw.uvarint(1) // bitmap len
	raw.u8(0)      // bitmap: not null
	raw.bytes(payload)

	dir := &directory{Schema: schema}
	bm := blockMeta{RowCount: 1}
	off := uint64(headerSize)
	statVals := []Value{Int64(42), String("x"), Float64(1), Bool(true)}
	for ci, col := range schema.Columns {
		sv := statVals[ci]
		if col.Type == KindBool {
			sv = Bool(false)
		}
		bm.Chunks = append(bm.Chunks, chunkMeta{
			Encoding:    enc,
			EncVersion:  encVersion,
			Compression: CompNone,
			Offset:      off,
			StoredLen:   uint64(len(raw.buf)),
			RawLen:      uint64(len(raw.buf)),
			CRC32:       crc32Of(raw.buf),
			Stats:       BlockStats{RowCount: 1, HasMinMax: true, Min: sv, Max: sv},
		})
		off += uint64(len(raw.buf))
	}
	dir.Blocks = append(dir.Blocks, bm)
	if mutate != nil {
		mutate(dir)
	}

	out := &byteWriter{}
	out.bytes(encodeHeader())
	for range schema.Columns {
		out.bytes(raw.buf)
	}
	dirBytes := encodeDirectory(dir)
	out.bytes(dirBytes)
	var b [4]byte
	putU32(b[:], uint32(len(dirBytes)))
	out.bytes(b[:])
	putU32(b[:], crc32Of(dirBytes))
	out.bytes(b[:])
	out.bytes(fileMagic)

	p := filepath.Join(t.TempDir(), "synthetic.cen")
	if err := os.WriteFile(p, out.buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnknownEncodingID(t *testing.T) {
	p := buildSyntheticFile(t, func(d *directory) {
		d.Blocks[0].Chunks[0].Encoding = Encoding(200)
	})
	r, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = r.ReadBlockColumn(0, 0)
	if !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("got %v, want ErrUnsupportedEncoding", err)
	}
}

func TestFutureEncodingVersion(t *testing.T) {
	p := buildSyntheticFile(t, func(d *directory) {
		d.Blocks[0].Chunks[0].EncVersion = encVersion + 1
	})
	r, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = r.ReadBlockColumn(0, 0)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v, want ErrUnsupportedVersion", err)
	}
}

func TestUnknownCompression(t *testing.T) {
	p := buildSyntheticFile(t, func(d *directory) {
		d.Blocks[0].Chunks[0].Compression = Compression(77)
	})
	r, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = r.ReadBlockColumn(0, 0)
	if !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("got %v, want ErrUnsupportedEncoding", err)
	}
}

// TestRandomMutationsNoPanic flips random bytes and ensures Open and
// reads always return errors instead of panicking.
func TestRandomMutationsNoPanic(t *testing.T) {
	path := writeSmallFile(t)
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := uint64(12345)
	rand := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int(seed>>33) % n
	}
	for i := 0; i < 200; i++ {
		data := make([]byte, len(orig))
		copy(data, orig)
		for j := 0; j < 1+rand(4); j++ {
			data[rand(len(data))] ^= byte(1 << uint(rand(8)))
		}
		p := filepath.Join(t.TempDir(), "mut.cen")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("panic on mutated input: %v", rec)
				}
			}()
			r, err := Open(p)
			if err != nil {
				return
			}
			defer r.Close()
			for b := 0; b < r.NumBlocks(); b++ {
				for c := 0; c < r.Schema().NumCols(); c++ {
					r.ReadBlockColumn(b, c) // error or success, never panic
				}
			}
		}()
	}
}
