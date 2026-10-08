package columnar

import "testing"

func TestStringCodecsRoundTrip(t *testing.T) {
	vals := []string{"apple", "banana", "apple", "", "cherry", "banana", "apple", "🍇"}
	for _, id := range []EncodingID{EncPlain, EncDict} {
		enc, err := encodeStrings(id, vals)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := decodeStrings(id, encVersion, enc, len(vals))
		if err != nil {
			t.Fatal(err)
		}
		if len(dec) != len(vals) {
			t.Fatalf("enc %d: %d values, want %d", id, len(dec), len(vals))
		}
		for i := range vals {
			if dec[i] != vals[i] {
				t.Fatalf("enc %d idx %d: got %q want %q", id, i, dec[i], vals[i])
			}
		}
	}
}

func TestChooseStringEncoding(t *testing.T) {
	rep := []string{"a", "b", "a", "b", "a", "b", "a", "b"}
	if got := chooseStringEncoding(rep); got != EncDict {
		t.Fatalf("got %d want dict", got)
	}
	uniq := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	if got := chooseStringEncoding(uniq); got != EncPlain {
		t.Fatalf("got %d want plain", got)
	}
}

func TestDecodeStringsCorrupt(t *testing.T) {
	if _, err := decodeStrings(EncPlain, encVersion, []byte{10, 0, 0, 0, 'x'}, 1); err == nil {
		t.Error("expected truncation error")
	}
	if _, err := decodeStrings(EncDict, encVersion, []byte{1, 0}, 1); err == nil {
		t.Error("expected truncation error")
	}
}
