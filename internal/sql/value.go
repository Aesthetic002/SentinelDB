// Package sql is SentinelDB's language layer: lexer, recursive-descent parser for
// the context-free grammar in docs/grammar.md, explicit parse tree, and translation
// to relational algebra. It holds no data and takes no locks.
package sql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind is the runtime type of a Value.
type Kind uint8

const (
	KindNull Kind = iota
	KindInt
	KindText
	KindBool
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "NULL"
	case KindInt:
		return "INT"
	case KindText:
		return "TEXT"
	case KindBool:
		return "BOOL"
	}
	return "?"
}

// Value is a single SQL datum. The zero Value is NULL.
type Value struct {
	K Kind   `json:"k,omitempty"`
	I int64  `json:"i,omitempty"`
	S string `json:"s,omitempty"`
	B bool   `json:"b,omitempty"`
}

// Null, Int, Text and Bool construct Values.
var Null = Value{}

func Int(i int64) Value   { return Value{K: KindInt, I: i} }
func Text(s string) Value { return Value{K: KindText, S: s} }
func Bool(b bool) Value   { return Value{K: KindBool, B: b} }

// IsNull reports whether v is NULL.
func (v Value) IsNull() bool { return v.K == KindNull }

// String renders v as a SQL literal.
func (v Value) String() string {
	switch v.K {
	case KindInt:
		return strconv.FormatInt(v.I, 10)
	case KindText:
		return "'" + strings.ReplaceAll(v.S, "'", "''") + "'"
	case KindBool:
		if v.B {
			return "TRUE"
		}
		return "FALSE"
	}
	return "NULL"
}

// Compare orders two non-NULL values of one type. It returns an error on a type
// mismatch rather than guessing at a coercion.
func Compare(a, b Value) (int, error) {
	if a.K != b.K {
		return 0, fmt.Errorf("cannot compare %s with %s", a.K, b.K)
	}
	switch a.K {
	case KindInt:
		switch {
		case a.I < b.I:
			return -1, nil
		case a.I > b.I:
			return 1, nil
		}
		return 0, nil
	case KindText:
		return strings.Compare(a.S, b.S), nil
	case KindBool:
		switch {
		case a.B == b.B:
			return 0, nil
		case !a.B:
			return -1, nil
		}
		return 1, nil
	}
	return 0, nil
}

// EncodeKey produces a printable string for primary-key values that sorts in value
// order. It is hex rather than raw bytes so it survives JSON encoding unchanged in
// the replicated log.
func EncodeKey(vals []Value) string {
	var b strings.Builder
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',') // sorts below every hex digit, so a prefix sorts first
		}
		switch v.K {
		case KindInt:
			fmt.Fprintf(&b, "i%016x", uint64(v.I)^(1<<63)) // flip the sign to sort negatives first
		case KindText:
			b.WriteByte('t')
			fmt.Fprintf(&b, "%x", v.S)
		case KindBool:
			if v.B {
				b.WriteString("b1")
			} else {
				b.WriteString("b0")
			}
		default:
			b.WriteByte('n')
		}
	}
	return b.String()
}

// addChecked and friends refuse to wrap around: silently wrapping a balance would be
// the worst possible failure mode for a database.
func addChecked(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, fmt.Errorf("integer overflow in %d + %d", a, b)
	}
	return a + b, nil
}

func subChecked(a, b int64) (int64, error) {
	if (b < 0 && a > math.MaxInt64+b) || (b > 0 && a < math.MinInt64+b) {
		return 0, fmt.Errorf("integer overflow in %d - %d", a, b)
	}
	return a - b, nil
}

func mulChecked(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, fmt.Errorf("integer overflow in %d * %d", a, b)
	}
	r := a * b
	if r/b != a {
		return 0, fmt.Errorf("integer overflow in %d * %d", a, b)
	}
	return r, nil
}
