package sql

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Expressions
// ---------------------------------------------------------------------------

// Expr is a scalar expression.
type Expr interface {
	exprNode()
	String() string
}

type (
	// Literal is a constant written directly in the query.
	Literal struct{ V Value }
	// Param is a ? placeholder. Its value is bound at evaluation time and is only
	// ever data: it is never spliced into query text, so it cannot change the query's
	// structure. N counts from 0 in textual order.
	Param struct{ N int }
	// ColRef names a column, optionally qualified by a table or alias.
	ColRef struct{ Table, Name string }
	// Unary is NOT or arithmetic negation.
	Unary struct {
		Op string
		X  Expr
	}
	// Binary is an arithmetic, comparison or logical operator.
	Binary struct {
		Op   string
		L, R Expr
	}
	// IsNull is "x IS [NOT] NULL".
	IsNull struct {
		X      Expr
		Negate bool
	}
)

func (Literal) exprNode() {}
func (Param) exprNode()   {}
func (ColRef) exprNode()  {}
func (Unary) exprNode()   {}
func (Binary) exprNode()  {}
func (IsNull) exprNode()  {}

func (e Literal) String() string { return e.V.String() }
func (e Param) String() string   { return fmt.Sprintf("$%d", e.N+1) }
func (e ColRef) String() string {
	if e.Table != "" {
		return e.Table + "." + e.Name
	}
	return e.Name
}
func (e Unary) String() string {
	if e.Op == "NOT" {
		return "NOT " + e.X.String()
	}
	return e.Op + e.X.String()
}
func (e Binary) String() string { return "(" + e.L.String() + " " + e.Op + " " + e.R.String() + ")" }
func (e IsNull) String() string {
	if e.Negate {
		return e.X.String() + " IS NOT NULL"
	}
	return e.X.String() + " IS NULL"
}

// Env supplies the values an expression refers to.
type Env interface {
	Column(ref ColRef) (Value, error)
	Param(n int) (Value, error)
}

// Eval computes an expression using SQL three-valued logic: comparing with NULL gives
// NULL ("unknown"), not false, and only a true WHERE condition passes a row.
func Eval(e Expr, env Env) (Value, error) {
	switch x := e.(type) {
	case Literal:
		return x.V, nil
	case Param:
		return env.Param(x.N)
	case ColRef:
		return env.Column(x)
	case IsNull:
		v, err := Eval(x.X, env)
		if err != nil {
			return Null, err
		}
		return Bool(v.IsNull() != x.Negate), nil
	case Unary:
		v, err := Eval(x.X, env)
		if err != nil || v.IsNull() {
			return Null, err
		}
		if x.Op == "NOT" {
			if v.K != KindBool {
				return Null, fmt.Errorf("NOT needs BOOL, got %s", v.K)
			}
			return Bool(!v.B), nil
		}
		if v.K != KindInt {
			return Null, fmt.Errorf("unary minus needs INT, got %s", v.K)
		}
		r, err := subChecked(0, v.I)
		return Int(r), err
	case Binary:
		return evalBinary(x, env)
	}
	return Null, fmt.Errorf("unknown expression %T", e)
}

func evalBinary(x Binary, env Env) (Value, error) {
	l, err := Eval(x.L, env)
	if err != nil {
		return Null, err
	}
	if x.Op == "AND" || x.Op == "OR" {
		return evalLogical(x, l, env)
	}
	r, err := Eval(x.R, env)
	if err != nil {
		return Null, err
	}
	if l.IsNull() || r.IsNull() {
		return Null, nil
	}
	switch x.Op {
	case "=", "<>", "<", "<=", ">", ">=":
		c, err := Compare(l, r)
		if err != nil {
			return Null, err
		}
		var res bool
		switch x.Op {
		case "=":
			res = c == 0
		case "<>":
			res = c != 0
		case "<":
			res = c < 0
		case "<=":
			res = c <= 0
		case ">":
			res = c > 0
		case ">=":
			res = c >= 0
		}
		return Bool(res), nil
	}
	if l.K != KindInt || r.K != KindInt {
		return Null, fmt.Errorf("%s needs INT operands, got %s and %s", x.Op, l.K, r.K)
	}
	var v int64
	switch x.Op {
	case "+":
		v, err = addChecked(l.I, r.I)
	case "-":
		v, err = subChecked(l.I, r.I)
	case "*":
		v, err = mulChecked(l.I, r.I)
	case "/":
		if r.I == 0 {
			return Null, fmt.Errorf("division by zero")
		}
		if l.I == -1<<63 && r.I == -1 {
			return Null, fmt.Errorf("integer overflow in %d / %d", l.I, r.I)
		}
		v = l.I / r.I
	default:
		return Null, fmt.Errorf("unknown operator %s", x.Op)
	}
	return Int(v), err
}

func evalLogical(x Binary, l Value, env Env) (Value, error) {
	if !l.IsNull() && l.K != KindBool {
		return Null, fmt.Errorf("%s needs BOOL, got %s", x.Op, l.K)
	}
	// Short circuit is safe: false AND anything is false; true OR anything is true.
	if !l.IsNull() && ((x.Op == "AND" && !l.B) || (x.Op == "OR" && l.B)) {
		return l, nil
	}
	r, err := Eval(x.R, env)
	if err != nil {
		return Null, err
	}
	if !r.IsNull() && r.K != KindBool {
		return Null, fmt.Errorf("%s needs BOOL, got %s", x.Op, r.K)
	}
	if !r.IsNull() && ((x.Op == "AND" && !r.B) || (x.Op == "OR" && r.B)) {
		return r, nil
	}
	if l.IsNull() || r.IsNull() {
		return Null, nil
	}
	return r, nil
}

// Conjuncts flattens nested ANDs, for finding primary-key lookups in a WHERE clause.
func Conjuncts(e Expr) []Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(Binary); ok && b.Op == "AND" {
		return append(Conjuncts(b.L), Conjuncts(b.R)...)
	}
	return []Expr{e}
}

// ColumnsIn lists every column an expression refers to.
func ColumnsIn(e Expr) []ColRef {
	switch x := e.(type) {
	case ColRef:
		return []ColRef{x}
	case Unary:
		return ColumnsIn(x.X)
	case IsNull:
		return ColumnsIn(x.X)
	case Binary:
		return append(ColumnsIn(x.L), ColumnsIn(x.R)...)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Relational algebra
// ---------------------------------------------------------------------------

// Plan is a relational-algebra expression. Each node corresponds to one operator, and
// String renders the whole query in standard notation.
type Plan interface {
	planNode()
	String() string
}

type (
	// Scan is a base relation, optionally renamed: rho_alias(table).
	Scan struct{ Table, Alias string }
	// Selection is sigma_cond(input): the rows satisfying a predicate.
	Selection struct {
		Cond  Expr
		Input Plan
	}
	// Projection is pi_cols(input). A nil Cols means every column.
	Projection struct {
		Cols  []ColRef
		Input Plan
	}
	// Join is the theta join left |x|_cond right.
	Join struct {
		Cond        Expr
		Left, Right Plan
	}
	// Sort is tau_key(input).
	Sort struct {
		Key   ColRef
		Desc  bool
		Input Plan
	}
	// Limit keeps the first N rows.
	Limit struct {
		N     int64
		Input Plan
	}
)

func (Scan) planNode()       {}
func (Selection) planNode()  {}
func (Projection) planNode() {}
func (Join) planNode()       {}
func (Sort) planNode()       {}
func (Limit) planNode()      {}

// Name returns the name a scanned table is referred to by.
func (s Scan) Name() string {
	if s.Alias != "" {
		return s.Alias
	}
	return s.Table
}

func (p Scan) String() string {
	if p.Alias != "" && p.Alias != p.Table {
		return "ρ[" + p.Alias + "](" + p.Table + ")"
	}
	return p.Table
}
func (p Selection) String() string { return "σ[" + p.Cond.String() + "](" + p.Input.String() + ")" }
func (p Projection) String() string {
	if p.Cols == nil {
		return p.Input.String()
	}
	cs := make([]string, len(p.Cols))
	for i, c := range p.Cols {
		cs[i] = c.String()
	}
	return "π[" + strings.Join(cs, ", ") + "](" + p.Input.String() + ")"
}
func (p Join) String() string {
	return "(" + p.Left.String() + " ⋈[" + p.Cond.String() + "] " + p.Right.String() + ")"
}
func (p Sort) String() string {
	dir := "ASC"
	if p.Desc {
		dir = "DESC"
	}
	return "τ[" + p.Key.String() + " " + dir + "](" + p.Input.String() + ")"
}
func (p Limit) String() string { return fmt.Sprintf("LIMIT[%d](%s)", p.N, p.Input) }
