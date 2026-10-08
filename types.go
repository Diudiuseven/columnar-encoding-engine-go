// Package columnar implements a columnar encoding and compression engine.
//
// Data is written in row blocks; each block stores every column as an
// independently encoded chunk. The on-disk format is versioned and
// deterministic: the same input and configuration always produce
// byte-identical output.
package columnar

import "fmt"

// Kind is the logical type of a column.
type Kind uint8

const (
	KindInvalid Kind = iota
	KindInt64
	KindFloat64
	KindString
	KindBool
)

func (k Kind) String() string {
	switch k {
	case KindInt64:
		return "int64"
	case KindFloat64:
		return "float64"
	case KindString:
		return "string"
	case KindBool:
		return "bool"
	default:
		return "invalid"
	}
}

// Value is a single cell. Null values carry Kind (the column type) and
// Null=true; other fields are zero.
type Value struct {
	Kind Kind
	Null bool
	I64  int64
	F64  float64
	Str  string
	Bool bool
}

func Int64(v int64) Value     { return Value{Kind: KindInt64, I64: v} }
func Float64(v float64) Value { return Value{Kind: KindFloat64, F64: v} }
func String(v string) Value   { return Value{Kind: KindString, Str: v} }
func Bool(v bool) Value       { return Value{Kind: KindBool, Bool: v} }

// NullOf returns a null Value of the given kind.
func NullOf(k Kind) Value { return Value{Kind: k, Null: true} }

// compareValues orders two non-null values of the same kind.
// It returns -1, 0 or 1. Callers must ensure a.Kind == b.Kind.
func compareValues(a, b Value) int {
	switch a.Kind {
	case KindInt64:
		switch {
		case a.I64 < b.I64:
			return -1
		case a.I64 > b.I64:
			return 1
		}
	case KindFloat64:
		switch {
		case a.F64 < b.F64:
			return -1
		case a.F64 > b.F64:
			return 1
		}
	case KindString:
		switch {
		case a.Str < b.Str:
			return -1
		case a.Str > b.Str:
			return 1
		}
	case KindBool:
		switch {
		case !a.Bool && b.Bool:
			return -1
		case a.Bool && !b.Bool:
			return 1
		}
	}
	return 0
}

// Column describes one column of the schema.
type Column struct {
	Name string
	Type Kind
}

// Schema is an ordered list of columns.
type Schema struct {
	Columns []Column
}

// ColIndex returns the index of the named column, or -1.
func (s Schema) ColIndex(name string) int {
	for i, c := range s.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// NumCols returns the number of columns.
func (s Schema) NumCols() int { return len(s.Columns) }

// validate checks the schema for structural problems.
func (s *Schema) validate() error {
	if len(s.Columns) == 0 {
		return fmt.Errorf("columnar: schema has no columns")
	}
	seen := map[string]bool{}
	for _, c := range s.Columns {
		if c.Name == "" {
			return fmt.Errorf("columnar: column with empty name")
		}
		if c.Type == KindInvalid {
			return fmt.Errorf("columnar: column %q has invalid type", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("columnar: duplicate column name %q", c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}
