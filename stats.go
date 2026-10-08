package columnar

import "fmt"

// Op is a comparison operator for predicates.
type Op uint8

const (
	OpEq Op = iota
	OpLt
	OpLe
	OpGt
	OpGe
	OpIsNull
	OpNotNull
)

func (o Op) String() string {
	switch o {
	case OpEq:
		return "="
	case OpLt:
		return "<"
	case OpLe:
		return "<="
	case OpGt:
		return ">"
	case OpGe:
		return ">="
	case OpIsNull:
		return "IS NULL"
	case OpNotNull:
		return "IS NOT NULL"
	}
	return fmt.Sprintf("op(%d)", uint8(o))
}

// Predicate filters rows of one column. Null values never match
// comparison predicates (SQL semantics); use OpIsNull/OpNotNull for them.
type Predicate struct {
	Column string
	Op     Op
	Val    Value // unused for OpIsNull / OpNotNull
}

// BlockStats summarizes one column chunk within a block.
type BlockStats struct {
	RowCount  uint32
	NullCount uint32
	HasMinMax bool // false when the chunk has no non-null values
	Min       Value
	Max       Value
}

// statsBuilder accumulates statistics incrementally.
type statsBuilder struct {
	rowCount  uint32
	nullCount uint32
	hasMinMax bool
	min, max  Value
}

func (b *statsBuilder) add(v Value) {
	b.rowCount++
	if v.Null {
		b.nullCount++
		return
	}
	if !b.hasMinMax {
		b.min, b.max = v, v
		b.hasMinMax = true
		return
	}
	if compareValues(v, b.min) < 0 {
		b.min = v
	}
	if compareValues(v, b.max) > 0 {
		b.max = v
	}
}

func (b *statsBuilder) finish() BlockStats {
	return BlockStats{
		RowCount:  b.rowCount,
		NullCount: b.nullCount,
		HasMinMax: b.hasMinMax,
		Min:       b.min,
		Max:       b.max,
	}
}

// mayMatch reports whether a block with these stats could contain a row
// matching p. A false result is authoritative: the block is guaranteed
// to contain no matching row. The column type must already be validated
// against p by the caller.
func (s BlockStats) mayMatch(p Predicate) (bool, error) {
	nonNull := s.RowCount - s.NullCount
	switch p.Op {
	case OpIsNull:
		return s.NullCount > 0, nil
	case OpNotNull:
		return nonNull > 0, nil
	}
	// Comparison predicates only match non-null values.
	if nonNull == 0 || !s.HasMinMax {
		return false, nil
	}
	if p.Val.Kind != s.Min.Kind {
		return false, fmt.Errorf("%w: predicate kind %s does not match column kind %s",
			ErrSchema, p.Val.Kind, s.Min.Kind)
	}
	cMin := compareValues(p.Val, s.Min) // p.Val vs min
	cMax := compareValues(p.Val, s.Max) // p.Val vs max
	switch p.Op {
	case OpEq:
		// Some value equals p.Val only possible if min <= val <= max.
		return cMin >= 0 && cMax <= 0, nil
	case OpLt:
		// Exists v < p.Val iff min < p.Val.
		return cMin > 0, nil
	case OpLe:
		return cMin >= 0, nil
	case OpGt:
		// Exists v > p.Val iff max > p.Val.
		return cMax < 0, nil
	case OpGe:
		return cMax <= 0, nil
	}
	return false, fmt.Errorf("%w: unknown predicate op %d", ErrSchema, uint8(p.Op))
}
