package columnar

import (
	"bytes"
	"math"
	"testing"
)

func roundTrip(t *testing.T, id EncodingID, vals []int64) {
	t.Helper()
	enc, err := encodeInts(id, vals)
	if err != nil {
		t.Fatalf("encode %d: %v", id, err)
	}
	dec, err := decodeInts(id, encVersion, enc, len(vals))
	if err != nil {
		t.Fatalf("decode %d: %v", id, err)
	}
	if len(dec) != len(vals) {
		t.Fatalf("enc %d: got %d values, want %d", id, len(dec), len(vals))
	}
	for i := range vals {
		if dec[i] != vals[i] {
			t.Fatalf("enc %d idx %d: got %d want %d", id, i, dec[i], vals[i])
		}
	}
	// Determinism: re-encode must be byte-identical.
	enc2, _ := encodeInts(id, vals)
	if !bytes.Equal(enc, enc2) {
		t.Fatalf("enc %d: non-deterministic output", id)
	}
}

func TestIntCodecsRoundTrip(t *testing.T) {
	constant := []int64{7, 7, 7, 7, 7}
	ap := []int64{10, 13, 16, 19, 22, 25}
	runs := []int64{1, 1, 1, 2, 2, 2, 3, 3, 3, 4, 4, 4}
	lowCard := []int64{100, 200, 100, 300, 200, 100, 300, 200, 100, 300, 200, 100}
	narrow := []int64{1000, 1005, 999, 1020, 1001, 998, 1050, 1002}
	wide := []int64{math.MinInt64, 0, math.MaxInt64, -42, 42, 1 << 60}
	neg := []int64{-5, -4, -3, -2, -1, 0, 1}

	roundTrip(t, EncPlain, wide)
	roundTrip(t, EncPlain, nil)
	roundTrip(t, EncBitPack, narrow)
	roundTrip(t, EncBitPack, neg)
	roundTrip(t, EncRLE, constant)
	roundTrip(t, EncRLE, runs)
	roundTrip(t, EncDict, lowCard)
	roundTrip(t, EncDelta, ap)
	roundTrip(t, EncDelta, neg)
}

func TestChooseIntEncoding(t *testing.T) {
	cases := []struct {
		vals []int64
		want EncodingID
	}{
		{[]int64{5, 5, 5, 5}, EncRLE},
		{[]int64{0, 2, 4, 6, 8}, EncDelta},
		{[]int64{1, 1, 2, 2, 3, 3, 4, 4}, EncRLE},
		{[]int64{9, 8, 9, 8, 9, 8, 9, 8, 7, 7, 9, 8}, EncDict},
		{[]int64{100, 101, 103, 102, 105, 104, 107, 106, 109, 108, 111, 110, 113, 112, 115, 114, 117}, EncBitPack},
		{[]int64{math.MinInt64, math.MaxInt64, 3, -7, 1 << 62, -(1 << 62), 11, 13}, EncPlain},
	}
	for _, c := range cases {
		got := chooseIntEncoding(c.vals)
		if got != c.want {
			t.Errorf("vals %v: got enc %d want %d", c.vals, got, c.want)
		}
		// Whatever was chosen must round-trip losslessly.
		roundTrip(t, got, c.vals)
	}
}

func TestDecodeCorrupt(t *testing.T) {
	if _, err := decodeInts(EncPlain, encVersion, []byte{1, 2, 3}, 1); err == nil {
		t.Error("plain: expected error on short input")
	}
	if _, err := decodeInts(EncBitPack, encVersion, []byte{1}, 1); err == nil {
		t.Error("bitpack: expected error on short header")
	}
	if _, err := decodeInts(EncRLE, encVersion, []byte{1, 2, 3}, 1); err == nil {
		t.Error("rle: expected error on bad length")
	}
	if _, err := decodeInts(EncDict, encVersion, []byte{2, 0, 0, 0}, 1); err == nil {
		t.Error("dict: expected error on truncated dict")
	}
	if _, err := decodeInts(EncDelta, encVersion, []byte{1}, 1); err == nil {
		t.Error("delta: expected error on short payload")
	}
	if _, err := decodeInts(EncodingID(99), encVersion, nil, 0); err == nil {
		t.Error("expected unknown-encoding error")
	}
	if _, err := decodeInts(EncPlain, encVersion+1, nil, 0); err == nil {
		t.Error("expected unsupported-version error")
	}
}
