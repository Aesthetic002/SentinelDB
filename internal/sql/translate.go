package sql

import "fmt"

// Statement is a parsed, translated SQL statement.
type Statement interface{ stmtNode() }

type (
	// ColumnDef describes one column of a new table.
	ColumnDef struct {
		Name    string
		Type    Kind
		NotNull bool
	}
	// ForeignKey is a single-column reference to another table's primary key.
	ForeignKey struct {
		Column, RefTable, RefColumn string
	}
	// CreateTable is CREATE TABLE.
	CreateTable struct {
		Name       string
		Columns    []ColumnDef
		PrimaryKey []string
		Foreign    []ForeignKey
	}
	// Insert is INSERT. A nil Columns means all columns in schema order.
	Insert struct {
		Table   string
		Columns []string
		Rows    [][]Expr
	}
	// Assignment is one "col = expr" of an UPDATE.
	Assignment struct {
		Column string
		Value  Expr
	}
	// Update is UPDATE.
	Update struct {
		Table string
		Set   []Assignment
		Where Expr
	}
	// Delete is DELETE.
	Delete struct {
		Table string
		Where Expr
	}
	// Select is SELECT, already expressed as relational algebra.
	Select struct{ Plan Plan }
	// Begin, Commit and Rollback are transaction boundaries.
	Begin    struct{}
	Commit   struct{}
	Rollback struct{}
	// Explain shows a statement's parse tree and algebra without running it.
	Explain struct {
		Inner Statement
		Tree  *ParseNode
	}
)

func (CreateTable) stmtNode() {}
func (Insert) stmtNode()      {}
func (Update) stmtNode()      {}
func (Delete) stmtNode()      {}
func (Select) stmtNode()      {}
func (Begin) stmtNode()       {}
func (Commit) stmtNode()      {}
func (Rollback) stmtNode()    {}
func (Explain) stmtNode()     {}

// AlgebraString renders the relational-algebra form of a DML statement. Inserts,
// updates and deletes are shown as the relation changes they perform.
func AlgebraString(s Statement) string {
	switch x := s.(type) {
	case Select:
		return x.Plan.String()
	case Insert:
		return fmt.Sprintf("%s := %s ∪ {%d new row(s)}", x.Table, x.Table, len(x.Rows))
	case Delete:
		if x.Where == nil {
			return x.Table + " := ∅"
		}
		return fmt.Sprintf("%s := %s − σ[%s](%s)", x.Table, x.Table, x.Where, x.Table)
	case Update:
		sets := ""
		for i, a := range x.Set {
			if i > 0 {
				sets += ", "
			}
			sets += a.Column + " ← " + a.Value.String()
		}
		if x.Where == nil {
			return fmt.Sprintf("%s := extend[%s](%s)", x.Table, sets, x.Table)
		}
		return fmt.Sprintf("%s := (%s − σ[%s](%s)) ∪ extend[%s](σ[%s](%s))",
			x.Table, x.Table, x.Where, x.Table, sets, x.Where, x.Table)
	}
	return ""
}

// ParseStatement runs the first two stages of the pipeline: text to parse tree, then
// parse tree to statement. It returns the number of ? placeholders.
func ParseStatement(src string) (Statement, int, error) {
	tree, params, err := Parse(src)
	if err != nil {
		return nil, 0, err
	}
	t := &translator{}
	st, err := t.statement(tree)
	if err != nil {
		return nil, 0, err
	}
	return st, params, nil
}

type translator struct{ param int }

// ident extracts an identifier from a leaf.
func ident(n *ParseNode) string { return n.Token.Text }

func (t *translator) statement(n *ParseNode) (Statement, error) {
	switch n.Symbol {
	case "txn_stmt":
		switch n.Children[0].Token.Text {
		case "BEGIN":
			return Begin{}, nil
		case "COMMIT":
			return Commit{}, nil
		}
		return Rollback{}, nil
	case "explain_stmt":
		inner, err := t.statement(n.Children[1])
		if err != nil {
			return nil, err
		}
		return Explain{Inner: inner, Tree: n.Children[1]}, nil
	case "create_stmt":
		return t.createTable(n)
	case "insert_stmt":
		return t.insert(n)
	case "select_stmt":
		return t.selectStmt(n)
	case "update_stmt":
		return t.update(n)
	case "delete_stmt":
		return t.deleteStmt(n)
	}
	return nil, fmt.Errorf("internal error: no translation for %s", n.Symbol)
}

func (t *translator) createTable(n *ParseNode) (Statement, error) {
	ct := CreateTable{Name: ident(n.child("IDENT"))}
	seen := map[string]bool{}
	for _, c := range n.Children {
		switch c.Symbol {
		case "column_def":
			col := ColumnDef{Name: ident(c.Children[0])}
			if seen[col.Name] {
				return nil, fmt.Errorf("column %s is defined twice", col.Name)
			}
			seen[col.Name] = true
			switch c.Children[1].Children[0].Token.Text {
			case "INT", "INTEGER":
				col.Type = KindInt
			case "TEXT":
				col.Type = KindText
			default:
				col.Type = KindBool
			}
			for _, k := range c.Children[2:] {
				switch k.Symbol {
				case "not_null":
					col.NotNull = true
				case "primary_key":
					if ct.PrimaryKey != nil {
						return nil, fmt.Errorf("table %s has more than one primary key", ct.Name)
					}
					ct.PrimaryKey = []string{col.Name}
				case "references":
					ids := k.all("IDENT")
					ct.Foreign = append(ct.Foreign, ForeignKey{col.Name, ident(ids[0]), ident(ids[1])})
				}
			}
			ct.Columns = append(ct.Columns, col)
		case "pk_constraint":
			if ct.PrimaryKey != nil {
				return nil, fmt.Errorf("table %s has more than one primary key", ct.Name)
			}
			for _, id := range c.child("ident_list").all("IDENT") {
				ct.PrimaryKey = append(ct.PrimaryKey, ident(id))
			}
		case "fk_constraint":
			ids := c.all("IDENT")
			ct.Foreign = append(ct.Foreign, ForeignKey{ident(ids[0]), ident(ids[1]), ident(ids[2])})
		}
	}
	if ct.PrimaryKey == nil {
		return nil, fmt.Errorf("table %s needs a PRIMARY KEY: every relation must have a key", ct.Name)
	}
	return ct, nil
}

func (t *translator) insert(n *ParseNode) (Statement, error) {
	ins := Insert{Table: ident(n.child("IDENT"))}
	if l := n.child("ident_list"); l != nil {
		for _, id := range l.all("IDENT") {
			ins.Columns = append(ins.Columns, ident(id))
		}
	}
	for _, r := range n.all("row") {
		var row []Expr
		for _, e := range r.all("expr") {
			x, err := t.expr(e)
			if err != nil {
				return nil, err
			}
			row = append(row, x)
		}
		ins.Rows = append(ins.Rows, row)
	}
	return ins, nil
}

func (t *translator) tableRef(n *ParseNode) Scan {
	ids := n.all("IDENT")
	s := Scan{Table: ident(ids[0])}
	if len(ids) > 1 {
		s.Alias = ident(ids[1])
	}
	return s
}

// selectStmt builds the algebra in textbook order, innermost first:
// joins, then sigma for WHERE, tau for ORDER BY, pi for the select list, then LIMIT.
func (t *translator) selectStmt(n *ParseNode) (Statement, error) {
	// The select list precedes FROM in the text, and translation walks ? placeholders
	// in textual order, so nothing here may number parameters out of order: the select
	// list holds only column names, so handling FROM and WHERE first is safe.
	var plan Plan = t.tableRef(n.child("table_ref"))
	names := map[string]bool{plan.(Scan).Name(): true}
	for _, j := range n.all("join_clause") {
		right := t.tableRef(j.child("table_ref"))
		if names[right.Name()] {
			return nil, fmt.Errorf("table name %s is used twice; give one of them an alias", right.Name())
		}
		names[right.Name()] = true
		cond, err := t.expr(j.child("expr"))
		if err != nil {
			return nil, err
		}
		plan = Join{Cond: cond, Left: plan, Right: right}
	}
	if w := n.child("where_clause"); w != nil {
		cond, err := t.expr(w.child("expr"))
		if err != nil {
			return nil, err
		}
		plan = Selection{Cond: cond, Input: plan}
	}
	if o := n.child("order_clause"); o != nil {
		plan = Sort{Key: t.columnRef(o.child("column_ref")), Desc: o.hasKeyword("DESC"), Input: plan}
	}
	var cols []ColRef
	for _, c := range n.child("select_list").all("column_ref") {
		cols = append(cols, t.columnRef(c))
	}
	plan = Projection{Cols: cols, Input: plan}
	if l := n.child("limit_clause"); l != nil {
		plan = Limit{N: l.Children[1].Token.Int, Input: plan}
	}
	return Select{Plan: plan}, nil
}

func (t *translator) update(n *ParseNode) (Statement, error) {
	u := Update{Table: ident(n.child("IDENT"))}
	for _, a := range n.all("assignment") {
		v, err := t.additive(a.child("additive"))
		if err != nil {
			return nil, err
		}
		u.Set = append(u.Set, Assignment{Column: ident(a.child("IDENT")), Value: v})
	}
	if w := n.child("where_clause"); w != nil {
		cond, err := t.expr(w.child("expr"))
		if err != nil {
			return nil, err
		}
		u.Where = cond
	}
	return u, nil
}

func (t *translator) deleteStmt(n *ParseNode) (Statement, error) {
	d := Delete{Table: ident(n.child("IDENT"))}
	if w := n.child("where_clause"); w != nil {
		cond, err := t.expr(w.child("expr"))
		if err != nil {
			return nil, err
		}
		d.Where = cond
	}
	return d, nil
}

func (t *translator) columnRef(n *ParseNode) ColRef {
	ids := n.all("IDENT")
	if len(ids) == 2 {
		return ColRef{Table: ident(ids[0]), Name: ident(ids[1])}
	}
	return ColRef{Name: ident(ids[0])}
}

// leftFold translates "a op b op c" into a left-associative tree, the shape the
// iterative grammar rules leave to the translator.
func (t *translator) leftFold(n *ParseNode, each func(*ParseNode) (Expr, error)) (Expr, error) {
	left, err := each(n.Children[0])
	if err != nil {
		return nil, err
	}
	for i := 1; i+1 < len(n.Children); i += 2 {
		right, err := each(n.Children[i+1])
		if err != nil {
			return nil, err
		}
		left = Binary{Op: n.Children[i].Token.Text, L: left, R: right}
	}
	return left, nil
}

func (t *translator) expr(n *ParseNode) (Expr, error)    { return t.leftFold(n, t.andExpr) }
func (t *translator) andExpr(n *ParseNode) (Expr, error) { return t.leftFold(n, t.notExpr) }

func (t *translator) notExpr(n *ParseNode) (Expr, error) {
	if len(n.Children) == 2 {
		x, err := t.notExpr(n.Children[1])
		if err != nil {
			return nil, err
		}
		return Unary{Op: "NOT", X: x}, nil
	}
	return t.comparison(n.Children[0])
}

func (t *translator) comparison(n *ParseNode) (Expr, error) {
	l, err := t.additive(n.Children[0])
	if err != nil || len(n.Children) == 1 {
		return l, err
	}
	if n.Children[1].Symbol == "KEYWORD" { // IS [NOT] NULL
		return IsNull{X: l, Negate: n.hasKeyword("NOT")}, nil
	}
	r, err := t.additive(n.Children[2])
	if err != nil {
		return nil, err
	}
	return Binary{Op: n.Children[1].Token.Text, L: l, R: r}, nil
}

func (t *translator) additive(n *ParseNode) (Expr, error) {
	return t.leftFold(n, t.multiplicative)
}

func (t *translator) multiplicative(n *ParseNode) (Expr, error) {
	return t.leftFold(n, t.unary)
}

func (t *translator) unary(n *ParseNode) (Expr, error) {
	if len(n.Children) == 2 {
		x, err := t.unary(n.Children[1])
		if err != nil {
			return nil, err
		}
		return Unary{Op: "-", X: x}, nil
	}
	return t.primary(n.Children[0])
}

func (t *translator) primary(n *ParseNode) (Expr, error) {
	c := n.Children[0]
	switch c.Symbol {
	case "INTEGER":
		return Literal{Int(c.Token.Int)}, nil
	case "STRING":
		return Literal{Text(c.Token.Text)}, nil
	case "PARAM":
		p := Param{N: t.param}
		t.param++
		return p, nil
	case "column_ref":
		return t.columnRef(c), nil
	case "KEYWORD":
		switch c.Token.Text {
		case "TRUE":
			return Literal{Bool(true)}, nil
		case "FALSE":
			return Literal{Bool(false)}, nil
		}
		return Literal{Null}, nil
	case "SYMBOL": // "(" expr ")"
		return t.expr(n.Children[1])
	}
	return nil, fmt.Errorf("internal error: unexpected primary %s", c.Symbol)
}
