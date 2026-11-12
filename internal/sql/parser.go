package sql

import (
	"fmt"
	"strings"
)

// ParseNode is a node of the explicit parse tree. Interior nodes are labelled with the
// grammar nonterminal they derive; leaves hold the matched token. Each method of parser
// below implements exactly one production of the grammar in docs/grammar.md.
type ParseNode struct {
	Symbol   string
	Token    *Token
	Children []*ParseNode
}

// child returns the first direct child with the given symbol, or nil.
func (n *ParseNode) child(symbol string) *ParseNode {
	for _, c := range n.Children {
		if c.Symbol == symbol {
			return c
		}
	}
	return nil
}

// all returns every direct child with the given symbol.
func (n *ParseNode) all(symbol string) []*ParseNode {
	var out []*ParseNode
	for _, c := range n.Children {
		if c.Symbol == symbol {
			out = append(out, c)
		}
	}
	return out
}

// hasKeyword reports whether a direct child is the given keyword.
func (n *ParseNode) hasKeyword(kw string) bool {
	for _, c := range n.Children {
		if c.Token != nil && c.Token.Kind == TokKeyword && c.Token.Text == kw {
			return true
		}
	}
	return false
}

// Pretty renders the tree with indentation, for EXPLAIN.
func (n *ParseNode) Pretty() string {
	var b strings.Builder
	n.pretty(&b, "", true, true)
	return b.String()
}

func (n *ParseNode) pretty(b *strings.Builder, prefix string, last, root bool) {
	if !root {
		b.WriteString(prefix)
		if last {
			b.WriteString("`- ")
			prefix += "   "
		} else {
			b.WriteString("|- ")
			prefix += "|  "
		}
	}
	if n.Token != nil {
		fmt.Fprintf(b, "%s\n", n.Token)
	} else {
		fmt.Fprintf(b, "%s\n", n.Symbol)
	}
	for i, c := range n.Children {
		c.pretty(b, prefix, i == len(n.Children)-1, false)
	}
}

// maxDepth bounds recursion so "((((((..." cannot overflow the stack.
const maxDepth = 200

type parser struct {
	toks   []Token
	pos    int
	depth  int
	params int
}

// SyntaxError reports a query that the grammar does not derive.
type SyntaxError struct {
	Pos int
	Msg string
}

func (e *SyntaxError) Error() string {
	if e.Pos < 0 { // the lexer's messages already say where
		return "syntax error " + e.Msg
	}
	return fmt.Sprintf("syntax error at byte %d: %s", e.Pos, e.Msg)
}

// Parse checks query text against the grammar and returns its parse tree, plus the
// number of ? placeholders. Exactly one statement is accepted, which also rules out the
// "'; DROP TABLE" stacked-statement form of injection at the grammar level.
func Parse(src string) (*ParseNode, int, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, 0, &SyntaxError{Pos: -1, Msg: err.Error()}
	}
	p := &parser{toks: toks}
	var tree *ParseNode
	func() {
		defer func() {
			if r := recover(); r != nil {
				if se, ok := r.(*SyntaxError); ok {
					err = se
					return
				}
				panic(r)
			}
		}()
		tree = p.statement()
		if p.peek().Text == ";" && p.peek().Kind == TokSymbol {
			p.next()
		}
		if p.peek().Kind != TokEOF {
			p.fail("expected end of statement, found %s (only one statement is allowed per request)", p.peek())
		}
	}()
	if err != nil {
		return nil, 0, err
	}
	return tree, p.params, nil
}

func (p *parser) peek() Token { return p.toks[p.pos] }
func (p *parser) next() Token {
	t := p.toks[p.pos]
	if t.Kind != TokEOF {
		p.pos++
	}
	return t
}

func (p *parser) fail(format string, args ...any) {
	panic(&SyntaxError{Pos: p.peek().Pos, Msg: fmt.Sprintf(format, args...)})
}

func leaf(t Token) *ParseNode {
	sym := "SYMBOL"
	switch t.Kind {
	case TokKeyword:
		sym = "KEYWORD"
	case TokIdent:
		sym = "IDENT"
	case TokInt:
		sym = "INTEGER"
	case TokString:
		sym = "STRING"
	case TokParam:
		sym = "PARAM"
	}
	return &ParseNode{Symbol: sym, Token: &t}
}

func (p *parser) isKW(kw string) bool {
	t := p.peek()
	return t.Kind == TokKeyword && t.Text == kw
}

func (p *parser) isSym(s string) bool {
	t := p.peek()
	return t.Kind == TokSymbol && t.Text == s
}

func (p *parser) kw(kw string) *ParseNode {
	if !p.isKW(kw) {
		p.fail("expected %s, found %s", kw, p.peek())
	}
	return leaf(p.next())
}

func (p *parser) sym(s string) *ParseNode {
	if !p.isSym(s) {
		p.fail("expected '%s', found %s", s, p.peek())
	}
	return leaf(p.next())
}

func (p *parser) ident() *ParseNode {
	if p.peek().Kind != TokIdent {
		p.fail("expected an identifier, found %s", p.peek())
	}
	return leaf(p.next())
}

func (p *parser) enter() func() {
	p.depth++
	if p.depth > maxDepth {
		p.fail("expression nested more than %d levels deep", maxDepth)
	}
	return func() { p.depth-- }
}

// statement -> create_stmt | insert_stmt | select_stmt | update_stmt | delete_stmt
//
//	| txn_stmt | explain_stmt
func (p *parser) statement() *ParseNode {
	t := p.peek()
	if t.Kind == TokKeyword {
		switch t.Text {
		case "CREATE":
			return p.createStmt()
		case "BEGIN", "COMMIT", "ROLLBACK":
			return &ParseNode{Symbol: "txn_stmt", Children: []*ParseNode{leaf(p.next())}}
		case "EXPLAIN":
			return &ParseNode{Symbol: "explain_stmt", Children: []*ParseNode{leaf(p.next()), p.dml()}}
		case "SELECT", "INSERT", "UPDATE", "DELETE":
			return p.dml()
		}
	}
	p.fail("expected a statement, found %s", t)
	return nil
}

// dml -> select_stmt | insert_stmt | update_stmt | delete_stmt
func (p *parser) dml() *ParseNode {
	switch {
	case p.isKW("SELECT"):
		return p.selectStmt()
	case p.isKW("INSERT"):
		return p.insertStmt()
	case p.isKW("UPDATE"):
		return p.updateStmt()
	case p.isKW("DELETE"):
		return p.deleteStmt()
	}
	p.fail("expected SELECT, INSERT, UPDATE or DELETE, found %s", p.peek())
	return nil
}

// create_stmt -> CREATE TABLE IDENT "(" table_element { "," table_element } ")"
func (p *parser) createStmt() *ParseNode {
	n := &ParseNode{Symbol: "create_stmt"}
	n.Children = append(n.Children, p.kw("CREATE"), p.kw("TABLE"), p.ident(), p.sym("("))
	n.Children = append(n.Children, p.tableElement())
	for p.isSym(",") {
		n.Children = append(n.Children, leaf(p.next()), p.tableElement())
	}
	n.Children = append(n.Children, p.sym(")"))
	return n
}

// table_element -> column_def | pk_constraint | fk_constraint
func (p *parser) tableElement() *ParseNode {
	switch {
	case p.isKW("PRIMARY"):
		return p.pkConstraint()
	case p.isKW("FOREIGN"):
		return p.fkConstraint()
	}
	return p.columnDef()
}

// column_def -> IDENT type { column_constraint }
// column_constraint -> NOT NULL | PRIMARY KEY | REFERENCES IDENT "(" IDENT ")"
func (p *parser) columnDef() *ParseNode {
	n := &ParseNode{Symbol: "column_def", Children: []*ParseNode{p.ident(), p.typeName()}}
	for {
		switch {
		case p.isKW("NOT"):
			n.Children = append(n.Children, &ParseNode{Symbol: "not_null",
				Children: []*ParseNode{leaf(p.next()), p.kw("NULL")}})
		case p.isKW("PRIMARY"):
			n.Children = append(n.Children, &ParseNode{Symbol: "primary_key",
				Children: []*ParseNode{leaf(p.next()), p.kw("KEY")}})
		case p.isKW("REFERENCES"):
			n.Children = append(n.Children, &ParseNode{Symbol: "references", Children: []*ParseNode{
				leaf(p.next()), p.ident(), p.sym("("), p.ident(), p.sym(")")}})
		default:
			return n
		}
	}
}

// type -> INT | INTEGER | TEXT | BOOL | BOOLEAN
func (p *parser) typeName() *ParseNode {
	t := p.peek()
	if t.Kind == TokKeyword {
		switch t.Text {
		case "INT", "INTEGER", "TEXT", "BOOL", "BOOLEAN":
			return &ParseNode{Symbol: "type", Children: []*ParseNode{leaf(p.next())}}
		}
	}
	p.fail("expected a column type (INT, TEXT, BOOL), found %s", t)
	return nil
}

// pk_constraint -> PRIMARY KEY "(" ident_list ")"
func (p *parser) pkConstraint() *ParseNode {
	return &ParseNode{Symbol: "pk_constraint", Children: []*ParseNode{
		p.kw("PRIMARY"), p.kw("KEY"), p.sym("("), p.identList(), p.sym(")")}}
}

// fk_constraint -> FOREIGN KEY "(" IDENT ")" REFERENCES IDENT "(" IDENT ")"
func (p *parser) fkConstraint() *ParseNode {
	return &ParseNode{Symbol: "fk_constraint", Children: []*ParseNode{
		p.kw("FOREIGN"), p.kw("KEY"), p.sym("("), p.ident(), p.sym(")"),
		p.kw("REFERENCES"), p.ident(), p.sym("("), p.ident(), p.sym(")")}}
}

// ident_list -> IDENT { "," IDENT }
func (p *parser) identList() *ParseNode {
	n := &ParseNode{Symbol: "ident_list", Children: []*ParseNode{p.ident()}}
	for p.isSym(",") {
		n.Children = append(n.Children, leaf(p.next()), p.ident())
	}
	return n
}

// insert_stmt -> INSERT INTO IDENT [ "(" ident_list ")" ] VALUES row { "," row }
func (p *parser) insertStmt() *ParseNode {
	n := &ParseNode{Symbol: "insert_stmt", Children: []*ParseNode{p.kw("INSERT"), p.kw("INTO"), p.ident()}}
	if p.isSym("(") {
		n.Children = append(n.Children, leaf(p.next()), p.identList(), p.sym(")"))
	}
	n.Children = append(n.Children, p.kw("VALUES"), p.row())
	for p.isSym(",") {
		n.Children = append(n.Children, leaf(p.next()), p.row())
	}
	return n
}

// row -> "(" expr { "," expr } ")"
func (p *parser) row() *ParseNode {
	n := &ParseNode{Symbol: "row", Children: []*ParseNode{p.sym("("), p.expr()}}
	for p.isSym(",") {
		n.Children = append(n.Children, leaf(p.next()), p.expr())
	}
	n.Children = append(n.Children, p.sym(")"))
	return n
}

// select_stmt -> SELECT select_list FROM table_ref { join_clause } [ where_clause ]
//
//	[ order_clause ] [ limit_clause ]
func (p *parser) selectStmt() *ParseNode {
	n := &ParseNode{Symbol: "select_stmt", Children: []*ParseNode{
		p.kw("SELECT"), p.selectList(), p.kw("FROM"), p.tableRef()}}
	for p.isKW("JOIN") || p.isKW("INNER") {
		n.Children = append(n.Children, p.joinClause())
	}
	if p.isKW("WHERE") {
		n.Children = append(n.Children, p.whereClause())
	}
	if p.isKW("ORDER") {
		n.Children = append(n.Children, p.orderClause())
	}
	if p.isKW("LIMIT") {
		n.Children = append(n.Children, p.limitClause())
	}
	return n
}

// select_list -> "*" | column_ref { "," column_ref }
func (p *parser) selectList() *ParseNode {
	n := &ParseNode{Symbol: "select_list"}
	if p.isSym("*") {
		n.Children = append(n.Children, leaf(p.next()))
		return n
	}
	n.Children = append(n.Children, p.columnRef())
	for p.isSym(",") {
		n.Children = append(n.Children, leaf(p.next()), p.columnRef())
	}
	return n
}

// table_ref -> IDENT [ [ AS ] IDENT ]
func (p *parser) tableRef() *ParseNode {
	n := &ParseNode{Symbol: "table_ref", Children: []*ParseNode{p.ident()}}
	if p.isKW("AS") {
		n.Children = append(n.Children, leaf(p.next()), p.ident())
	} else if p.peek().Kind == TokIdent {
		n.Children = append(n.Children, p.ident())
	}
	return n
}

// join_clause -> [ INNER ] JOIN table_ref ON expr
func (p *parser) joinClause() *ParseNode {
	n := &ParseNode{Symbol: "join_clause"}
	if p.isKW("INNER") {
		n.Children = append(n.Children, leaf(p.next()))
	}
	n.Children = append(n.Children, p.kw("JOIN"), p.tableRef(), p.kw("ON"), p.expr())
	return n
}

// where_clause -> WHERE expr
func (p *parser) whereClause() *ParseNode {
	return &ParseNode{Symbol: "where_clause", Children: []*ParseNode{p.kw("WHERE"), p.expr()}}
}

// order_clause -> ORDER BY column_ref [ ASC | DESC ]
func (p *parser) orderClause() *ParseNode {
	n := &ParseNode{Symbol: "order_clause", Children: []*ParseNode{p.kw("ORDER"), p.kw("BY"), p.columnRef()}}
	if p.isKW("ASC") || p.isKW("DESC") {
		n.Children = append(n.Children, leaf(p.next()))
	}
	return n
}

// limit_clause -> LIMIT INTEGER
func (p *parser) limitClause() *ParseNode {
	n := &ParseNode{Symbol: "limit_clause", Children: []*ParseNode{p.kw("LIMIT")}}
	if p.peek().Kind != TokInt {
		p.fail("LIMIT needs an integer, found %s", p.peek())
	}
	n.Children = append(n.Children, leaf(p.next()))
	return n
}

// update_stmt -> UPDATE IDENT SET assignment { "," assignment } [ where_clause ]
func (p *parser) updateStmt() *ParseNode {
	n := &ParseNode{Symbol: "update_stmt", Children: []*ParseNode{p.kw("UPDATE"), p.ident(), p.kw("SET"), p.assignment()}}
	for p.isSym(",") {
		n.Children = append(n.Children, leaf(p.next()), p.assignment())
	}
	if p.isKW("WHERE") {
		n.Children = append(n.Children, p.whereClause())
	}
	return n
}

// assignment -> IDENT "=" additive
func (p *parser) assignment() *ParseNode {
	return &ParseNode{Symbol: "assignment", Children: []*ParseNode{p.ident(), p.sym("="), p.additive()}}
}

// delete_stmt -> DELETE FROM IDENT [ where_clause ]
func (p *parser) deleteStmt() *ParseNode {
	n := &ParseNode{Symbol: "delete_stmt", Children: []*ParseNode{p.kw("DELETE"), p.kw("FROM"), p.ident()}}
	if p.isKW("WHERE") {
		n.Children = append(n.Children, p.whereClause())
	}
	return n
}

// The expression grammar below is the disambiguated form described in docs/grammar.md.
// Each precedence level is its own nonterminal, so there is exactly one parse for any
// expression, and left recursion has been replaced by iteration so that recursive
// descent terminates. Comparison is deliberately non-associative: "a < b < c" has no
// derivation at all and is rejected, rather than quietly meaning "(a < b) < c".
//
// expr       -> and_expr { OR and_expr }
// and_expr   -> not_expr { AND not_expr }
// not_expr   -> NOT not_expr | comparison
// comparison -> additive [ comp_op additive | IS [ NOT ] NULL ]
// additive   -> multiplicative { ( "+" | "-" ) multiplicative }
// multiplicative -> unary { ( "*" | "/" ) unary }
// unary      -> "-" unary | primary
// primary    -> INTEGER | STRING | TRUE | FALSE | NULL | PARAM | column_ref | "(" expr ")"
func (p *parser) expr() *ParseNode {
	defer p.enter()()
	n := &ParseNode{Symbol: "expr", Children: []*ParseNode{p.andExpr()}}
	for p.isKW("OR") {
		n.Children = append(n.Children, leaf(p.next()), p.andExpr())
	}
	return n
}

func (p *parser) andExpr() *ParseNode {
	n := &ParseNode{Symbol: "and_expr", Children: []*ParseNode{p.notExpr()}}
	for p.isKW("AND") {
		n.Children = append(n.Children, leaf(p.next()), p.notExpr())
	}
	return n
}

func (p *parser) notExpr() *ParseNode {
	defer p.enter()()
	if p.isKW("NOT") {
		return &ParseNode{Symbol: "not_expr", Children: []*ParseNode{leaf(p.next()), p.notExpr()}}
	}
	return &ParseNode{Symbol: "not_expr", Children: []*ParseNode{p.comparison()}}
}

var compOps = map[string]bool{"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

func (p *parser) comparison() *ParseNode {
	n := &ParseNode{Symbol: "comparison", Children: []*ParseNode{p.additive()}}
	switch {
	case p.peek().Kind == TokSymbol && compOps[p.peek().Text]:
		n.Children = append(n.Children, leaf(p.next()), p.additive())
		if t := p.peek(); t.Kind == TokSymbol && compOps[t.Text] {
			p.fail("comparisons cannot be chained: use AND, as in \"a < b AND b < c\"")
		}
	case p.isKW("IS"):
		n.Children = append(n.Children, leaf(p.next()))
		if p.isKW("NOT") {
			n.Children = append(n.Children, leaf(p.next()))
		}
		n.Children = append(n.Children, p.kw("NULL"))
	}
	return n
}

func (p *parser) additive() *ParseNode {
	n := &ParseNode{Symbol: "additive", Children: []*ParseNode{p.multiplicative()}}
	for p.isSym("+") || p.isSym("-") {
		n.Children = append(n.Children, leaf(p.next()), p.multiplicative())
	}
	return n
}

func (p *parser) multiplicative() *ParseNode {
	n := &ParseNode{Symbol: "multiplicative", Children: []*ParseNode{p.unary()}}
	for p.isSym("*") || p.isSym("/") {
		n.Children = append(n.Children, leaf(p.next()), p.unary())
	}
	return n
}

func (p *parser) unary() *ParseNode {
	defer p.enter()()
	if p.isSym("-") {
		return &ParseNode{Symbol: "unary", Children: []*ParseNode{leaf(p.next()), p.unary()}}
	}
	return &ParseNode{Symbol: "unary", Children: []*ParseNode{p.primary()}}
}

func (p *parser) primary() *ParseNode {
	n := &ParseNode{Symbol: "primary"}
	t := p.peek()
	switch {
	case t.Kind == TokInt || t.Kind == TokString:
		n.Children = append(n.Children, leaf(p.next()))
	case t.Kind == TokParam:
		p.params++
		n.Children = append(n.Children, leaf(p.next()))
	case t.Kind == TokKeyword && (t.Text == "TRUE" || t.Text == "FALSE" || t.Text == "NULL"):
		n.Children = append(n.Children, leaf(p.next()))
	case t.Kind == TokIdent:
		n.Children = append(n.Children, p.columnRef())
	case p.isSym("("):
		n.Children = append(n.Children, leaf(p.next()), p.expr(), p.sym(")"))
	default:
		p.fail("expected a value or column, found %s", t)
	}
	return n
}

// column_ref -> IDENT [ "." IDENT ]
func (p *parser) columnRef() *ParseNode {
	n := &ParseNode{Symbol: "column_ref", Children: []*ParseNode{p.ident()}}
	if p.isSym(".") {
		n.Children = append(n.Children, leaf(p.next()), p.ident())
	}
	return n
}
