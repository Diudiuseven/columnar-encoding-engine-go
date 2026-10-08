package columnar

import "testing"

func TestMayMatch(t *testing.T) {
	st := Stats{Min: int64(10), Max: int64(20), NullCount: 2, NumRows: 10}
	cases := []struct {
		p    Predicate
		want bool
	}{
		{Predicate{Op: Eq, Value: int64(15)}, true},
		{Predicate{Op: Eq, Value: int64(5)}, false},
		{Predicate{Op: Eq, Value: int64(25)}, false},
		{Predicate{Op: Ne, Value: int64(15)}, true},
		{Predicate{Op: Lt, Value: int64(10)}, false},
		{Predicate{Op: Le, Value: int64(10)}, true},
		{Predicate{Op: Gt, Value: int64(20)}, false},
		{Predicate{Op: Ge, Value: int64(20)}, true},
		{Predicate{Op: IsNull}, true},
		{Predicate{Op: NotNull}, true},
	}
	for _, c := range cases {
		got, err := st.mayMatch(Int64Type, c.p)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("pred %+v: got %v want %v", c.p, got, c.want)
		}
	}
}

func TestMayMatchAllEqual(t *testing.T) {
	st := Stats{Min: "x", Max: "x", NullCount: 0, NumRows: 5}
	got, _ := st.mayMatch(StringType, Predicate{Op: Ne, Value: "x"})
	if got {
		t.Error("Ne on all-equal chunk must not match")
	}
	got, _ = st.mayMatch(StringType, Predicate{Op: Eq, Value: "x"})
	if !got {
		t.Error("Eq on all-equal chunk must match")
	}
}

func TestMayMatchAllNull(t *testing.T) {
	st := Stats{NullCount: 4, NumRows: 4}
	got, _ := st.mayMatch(Int64Type, Predicate{Op: IsNull})
	if !got {
		t.Error("IsNull must match all-null chunk")
	}
	for _, op := range []Op{Eq, Ne, Lt, Le, Gt, Ge, NotNull} {
		got, _ := st.mayMatch(Int64Type, Predicate{Op: op, Value: int64(1)})
		if got {
			t.Errorf("op %d must not match all-null chunk", op)
		}
	}
}

func TestComputeStats(t *testing.T) {
	c := &ColumnBatch{
		Nulls: []bool{false, true, false, false},
		Ints:  []int64{3, -1, 7},
	}
	st := computeStats(Int64Type, c)
	if st.NullCount != 1 || st.NumRows != 4 {
		t.Fatalf("bad counts: %+v", st)
	}
	if st.Min.(int64) != -1 || st.Max.(int64) != 7 {
		t.Fatalf("bad min/max: %+v", st)
	}
}
