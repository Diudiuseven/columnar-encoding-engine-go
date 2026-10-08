package columnar

import "testing"

func TestBitPackRoundTrip(t *testing.T) {
	vals := []uint64{0, 1, 2, 3, 3, 2, 1, 0, 7, 5}
	for _, width := range []uint{1, 2, 3, 5, 8, 13, 32, 64} {
		var w bitWriter
		var max uint64
		if width < 64 {
			max = 1<<width - 1
		} else {
			max = ^uint64(0)
		}
		for _, v := range vals {
			w.write(v&max, width)
		}
		data := w.bytes()
		r := bitReader{buf: data}
		for i, want := range vals {
			got, ok := r.read(width)
			if !ok {
				t.Fatalf("width %d idx %d: unexpected end", width, i)
			}
			if got != want&max {
				t.Fatalf("width %d idx %d: got %d want %d", width, i, got, want&max)
			}
		}
	}
}

func TestBitReaderTruncated(t *testing.T) {
	r := bitReader{buf: []byte{0x01}}
	if _, ok := r.read(9); ok {
		t.Fatal("expected failure reading past end")
	}
}
