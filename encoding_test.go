package columnar

import (
	"math"
	"testing"
)

func valueEq(a, b Value) bool {
	if a.Kind != b.Kind || a.Null != b.Null {
		return false
	}
	if a.Null {
		return true
	}
	switch a.Kind {
	case KindInt64:
		return a.I64 == b.I64
	case KindFloat64:
		return math.Float64bits(a.F64) == math.Float64bits(b.F64) ||
			(math.IsNaN(a.F64) && math.IsNaN(b.F64))
	case KindString:
		return a.Str == b.Str
	case KindBool:
		return a.Bool == b.Bool
	}
	return false
}

func roundTrip(t *testing.T, enc Encoding, vals []Value) {
	t.Helper()
	kind := KindInt64
	if len(vals) > 0 {
		kind = vals[0].Kind
	}
	data, err := encodeValues(enc, vals)
	if err != nil {
		t.Fatalf("encode %s: %v", enc, err)
	}
	out, err := decodeValues(enc, encVersion, data, len(vals), kind)
	if err != nil {
		t.Fatalf("decode %s: %v", enc, err)
	}
	if len(out) != len(vals) {
		t.Fatalf("%s: got %d values, want %d", enc, len(out), len(vals))
	}
	for i := range vals {
		if !valueEq(out[i], vals[i]) {
			t.Fatalf("%s: value %d mismatch: got %+v want %+v", enc, i, out[i], vals[i])
		}
	}
}

func TestPlainRoundTrip(t *testing.T) {
	roundTrip(t, EncPlain, []Value{Int64(1), Int64(-2), Int64(math.MinInt64), Int64(math.MaxInt64)})
	roundTrip(t, EncPlain, []Value{Float64(0), Float64(-0.0), Float64(math.NaN()), Float64(math.Inf(1)), Float64(math.SmallestNonzeroFloat64)})
	roundTrip(t, EncPlain, []Value{String(""), String("a"), String("hello 世界")})
	roundTrip(t, EncPlain, []Value{Bool(true), Bool(false), Bool(true)})
	roundTrip(t, EncPlain, nil)
}

func TestRLERoundTrip(t *testing.T) {
	roundTrip(t, EncRLE, []Value{Int64(7), Int64(7), Int64(7), Int64(8), Int64(8), Int64(9)})
	roundTrip(t, EncRLE, []Value{String("x")})
	roundTrip(t, EncRLE, nil)
	roundTrip(t, EncRLE, []Value{Float64(1.5), Float64(1.5), Float64(-0.0), Float64(0.0)})
}

func TestBitpackRoundTrip(t *testing.T) {
	roundTrip(t, EncBitpack, []Value{Int64(0)})
	roundTrip(t, EncBitpack, []Value{Int64(5), Int64(3), Int64(9), Int64(3)})
	roundTrip(t, EncBitpack, []Value{Int64(math.MinInt64), Int64(math.MaxInt64), Int64(0)})
	roundTrip(t, EncBitpack, []Value{Int64(-5), Int64(-3), Int64(-9)})
	roundTrip(t, EncBitpack, nil)
	// width 64 path
	roundTrip(t, EncBitpack, []Value{Int64(math.MinInt64), Int64(math.MaxInt64)})
}

func TestArithRoundTrip(t *testing.T) {
	roundTrip(t, EncArith, []Value{Int64(10), Int64(20), Int64(30), Int64(40)})
	roundTrip(t, EncArith, []Value{Int64(42)})
	roundTrip(t, EncArith, nil)
	roundTrip(t, EncArith, []Value{Int64(5), Int64(5), Int64(5)})
	// overflow-wrapping sequence: step*index wraps past MaxInt64
	roundTrip(t, EncArith, []Value{Int64(math.MaxInt64 - 1), Int64(math.MinInt64), Int64(math.MinInt64 + 2)})
	if _, _, ok := arithFits([]Value{Int64(1), Int64(2), Int64(4)}); ok {
		t.Fatal("non-arithmetic sequence reported as fitting")
	}
}

func TestDictRoundTrip(t *testing.T) {
	roundTrip(t, EncDict, []Value{String("a"), String("b"), String("a"), String("c"), String("b")})
	roundTrip(t, EncDict, []Value{Int64(1), Int64(1), Int64(2)})
	roundTrip(t, EncDict, []Value{String("only")})
	roundTrip(t, EncDict, nil)
	roundTrip(t, EncDict, []Value{Float64(math.NaN()), Float64(math.NaN()), Float64(-0.0), Float64(0.0)})
	roundTrip(t, EncDict, []Value{Bool(true), Bool(false), Bool(true)})
}

func TestDecodeRejectsCorruption(t *testing.T) {
	data, err := encodeValues(EncPlain, []Value{Int64(1), Int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	// Truncated.
	if _, err := decodeValues(EncPlain, encVersion, data[:len(data)-1], 2, KindInt64); err == nil {
		t.Fatal("truncated plain payload decoded without error")
	}
	// Trailing garbage.
	if _, err := decodeValues(EncPlain, encVersion, append(data, 0xff), 2, KindInt64); err == nil {
		t.Fatal("trailing bytes accepted")
	}
	// Unknown encoding.
	if _, err := decodeValues(Encoding(99), 1, data, 2, KindInt64); err == nil {
		t.Fatal("unknown encoding accepted")
	}
	// Future version.
	if _, err := decodeValues(EncPlain, encVersion+1, data, 2, KindInt64); err == nil {
		t.Fatal("future encoding version accepted")
	}
	// Corrupt RLE run count.
	if _, err := decodeValues(EncRLE, encVersion, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0}, 1, KindInt64); err == nil {
		t.Fatal("zero run length accepted")
	}
}

func TestEncodeDeterministic(t *testing.T) {
	vals := []Value{String("b"), String("a"), String("b"), String("c"), String("a")}
	d1, err := encodeValues(EncDict, vals)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := encodeValues(EncDict, vals)
	if err != nil {
		t.Fatal(err)
	}
	if string(d1) != string(d2) {
		t.Fatal("dict encoding not deterministic")
	}
}
