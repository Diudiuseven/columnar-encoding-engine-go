package columnar

import (
	"fmt"
	"math"
)

// Stats summarises one column chunk for predicate pushdown.
type Stats struct {
	Min       any // int64, float64 or string; nil when all values are null
	Max       any
	NullCount int64
	NumRows   int64
}

// compareValues compares two values of the given column type.
// It returns -1, 0 or 1. Values must be non-nil and of the column type.
func compareValues(typ Type, a, b any) int {
	switch typ {
	case Int64Type:
		x, y := a.(int64), b.(int64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case Float64Type:
		// Total order: NaN is equal to NaN and sorts below every
		// other value, so statistics and predicates stay consistent.
		x, y := a.(float64), b.(float64)
		xn, yn := math.IsNaN(x), math.IsNaN(y)
		switch {
		case xn && yn:
			return 0
		case xn:
			return -1
		case yn:
			return 1
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case StringType:
		x, y := a.(string), b.(string)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	return 0
}

// mayMatch reports whether a chunk with these statistics could contain a
// row satisfying p. It must never return false when a matching row
// exists. Null rows only satisfy IsNull; every comparison against a
// value is false for nulls.
func (s Stats) mayMatch(typ Type, p Predicate) (bool, error) {
	nonNull := s.NumRows - s.NullCount
	switch p.Op {
	case IsNull:
		return s.NullCount > 0, nil
	case NotNull:
		return nonNull > 0, nil
	}
	if nonNull == 0 {
		return false, nil
	}
	if p.Value == nil {
		return false, fmt.Errorf("%w: predicate %d needs a value", ErrSchema, p.Op)
	}
	// Validate the predicate value type matches the column type.
	switch typ {
	case Int64Type:
		if _, ok := p.Value.(int64); !ok {
			return false, fmt.Errorf("%w: predicate value must be int64", ErrSchema)
		}
	case Float64Type:
		if _, ok := p.Value.(float64); !ok {
			return false, fmt.Errorf("%w: predicate value must be float64", ErrSchema)
		}
	case StringType:
		if _, ok := p.Value.(string); !ok {
			return false, fmt.Errorf("%w: predicate value must be string", ErrSchema)
		}
	}
	cmpMin := compareValues(typ, s.Min, p.Value)
	cmpMax := compareValues(typ, s.Max, p.Value)
	switch p.Op {
	case Eq:
		// v is possible only within [min, max].
		return cmpMin <= 0 && cmpMax >= 0, nil
	case Ne:
		// impossible only if every non-null value equals v.
		return !(cmpMin == 0 && cmpMax == 0), nil
	case Lt:
		return cmpMin < 0, nil
	case Le:
		return cmpMin <= 0, nil
	case Gt:
		return cmpMax > 0, nil
	case Ge:
		return cmpMax >= 0, nil
	}
	return false, fmt.Errorf("%w: unknown op %d", ErrSchema, p.Op)
}

// computeStats builds statistics for one column batch.
func computeStats(typ Type, c *ColumnBatch) Stats {
	st := Stats{NumRows: int64(len(c.Nulls))}
	for _, n := range c.Nulls {
		if n {
			st.NullCount++
		}
	}
	switch typ {
	case Int64Type:
		if len(c.Ints) > 0 {
			lo, hi := c.Ints[0], c.Ints[0]
			for _, v := range c.Ints[1:] {
				if v < lo {
					lo = v
				}
				if v > hi {
					hi = v
				}
			}
			st.Min, st.Max = lo, hi
		}
	case Float64Type:
		if len(c.Floats) > 0 {
			lo, hi := c.Floats[0], c.Floats[0]
			for _, v := range c.Floats[1:] {
				if compareValues(Float64Type, v, lo) < 0 {
					lo = v
				}
				if compareValues(Float64Type, v, hi) > 0 {
					hi = v
				}
			}
			st.Min, st.Max = lo, hi
		}
	case StringType:
		if len(c.Strs) > 0 {
			lo, hi := c.Strs[0], c.Strs[0]
			for _, v := range c.Strs[1:] {
				if v < lo {
					lo = v
				}
				if v > hi {
					hi = v
				}
			}
			st.Min, st.Max = lo, hi
		}
	}
	return st
}
