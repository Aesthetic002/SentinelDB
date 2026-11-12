package sql

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func mustParse(t *testing.T, q string) Statement {
	t.Helper()
	st, _, err := ParseStatement(q)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	return st
}

func TestValidStatementsParse(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t",
		"select a, b from t where a = 1;",
		"SELECT t.a FROM t AS x",
		"SELECT o.id, c.name FROM orders o JOIN customers c ON o.customer_id = c.id WHERE c.id = 42",
		"SELECT a FROM t INNER JOIN u ON t.x = u.y JOIN v ON u.z = v.z ORDER BY a DESC LIMIT 5",
		"INSERT INTO t VALUES (1, 'x', TRUE, NULL)",
		"INSERT INTO t (a, b) VALUES (?, ?), (?, ?)",
		"UPDATE t SET a = a + 1, b = 'y' WHERE id = 3",
		"UPDATE accounts SET balance = balance - ? WHERE id = ?",
		"DELETE FROM t WHERE a IS NOT NULL",
		"DELETE FROM t",
		"CREATE TABLE t (id INT PRIMARY KEY, name TEXT NOT NULL, ok BOOL)",
		"CREATE TABLE t (a INT, b INT, c TEXT, PRIMARY KEY (a, b), FOREIGN KEY (a) REFERENCES p (id))",
		"CREATE TABLE t (id INT PRIMARY KEY, p INT REFERENCES parent (id))",
		"BEGIN", "COMMIT;", "ROLLBACK",
		"EXPLAIN SELECT * FROM t WHERE a = 1 -- trailing comment",
		"SELECT * FROM t WHERE NOT (a = 1 OR b = 2) AND c <> 3 AND d != 4",
		"SELECT * FROM t WHERE a = -1 AND b = (1 + 2) * 3",
	} {
		mustParse(t, q)
	}
}

func TestInvalidStatementsAreRejected(t *testing.T) {
	for _, q := range []string{
		"", "   ", "SELEC * FROM t", "SELECT FROM t", "SELECT * t", "SELECT * FROM",
		"SELECT * FROM t WHERE", "SELECT * FROM t WHERE a =", "SELECT a, FROM t",
		"SELECT * FROM t JOIN u", "INSERT INTO t", "INSERT t VALUES (1)", "INSERT INTO t VALUES ()",
		"UPDATE t", "UPDATE t SET a", "DELETE t", "CREATE TABLE t ()", "CREATE TABLE t (a)",
		"CREATE TABLE t (a BLOB PRIMARY KEY)", "CREATE TABLE t (a INT)",
		"SELECT * FROM t LIMIT x", "SELECT 'unterminated FROM t", "SELECT * FROM t WHERE a = 99999999999999999999",
		"SELECT * FROM t WHERE 1a = 1", "SELECT @ FROM t", "SELECT * FROM t;;", "EXPLAIN BEGIN",
		"CREATE TABLE t (a INT, a INT, PRIMARY KEY (a))",
		"CREATE TABLE t (a INT PRIMARY KEY, b INT PRIMARY KEY)",
	} {
		if _, _, err := ParseStatement(q); err == nil {
			t.Errorf("%q should have been rejected", q)
		}
	}
}

// The grammar's central claim: precedence comes from its structure, and every expression
// has exactly one parse. Each case states the tree that structure must produce.
func TestPrecedenceAndAssociativity(t *testing.T) {
	cases := map[string]string{
		"a = 1 OR b = 2 AND c = 3": "((a = 1) OR ((b = 2) AND (c = 3)))",
		"a = 1 AND b = 2 OR c = 3": "(((a = 1) AND (b = 2)) OR (c = 3))",
		"NOT a = 1 AND b = 2":      "(NOT (a = 1) AND (b = 2))",
		"NOT NOT a = 1":            "NOT NOT (a = 1)",
		"1 + 2 * 3 = 7":            "((1 + (2 * 3)) = 7)",
		"(1 + 2) * 3 = 9":          "(((1 + 2) * 3) = 9)",
		"1 - 2 - 3 = 0":            "(((1 - 2) - 3) = 0)", // left associative, not 1-(2-3)
		"8 / 4 / 2 = 1":            "(((8 / 4) / 2) = 1)",
		"a + 1 < b * 2":            "((a + 1) < (b * 2))",
		"- a = 1":                  "(-a = 1)",
	}
	for src, want := range cases {
		st := mustParse(t, "SELECT * FROM t WHERE "+src)
		plan := st.(Select).Plan.(Projection).Input.(Selection)
		if got := plan.Cond.String(); got != want {
			t.Errorf("%s\n  parsed as %s\n  want      %s", src, got, want)
		}
	}
}

func TestChainedComparisonHasNoDerivation(t *testing.T) {
	for _, q := range []string{"a < b < c", "a = b = c", "a = 1 <> 2", "1 < 2 >= 3"} {
		_, _, err := ParseStatement("SELECT * FROM t WHERE " + q)
		var se *SyntaxError
		if !errors.As(err, &se) || !strings.Contains(se.Msg, "chained") {
			t.Errorf("%q: expected a chained-comparison syntax error, got %v", q, err)
		}
	}
	// With explicit grouping there is no ambiguity, so it is allowed.
	mustParse(t, "SELECT * FROM t WHERE (a < b) = TRUE")
}

// Adversarial input must produce an error, never a crash.
func TestDeepNestingIsBounded(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t WHERE " + strings.Repeat("(", 100000) + "1" + strings.Repeat(")", 100000),
		"SELECT * FROM t WHERE " + strings.Repeat("NOT ", 100000) + "a",
		"SELECT * FROM t WHERE a = " + strings.Repeat("- ", 100000) + "1",
	} {
		if _, _, err := ParseStatement(q); err == nil {
			t.Errorf("deeply nested query of %d bytes should be rejected", len(q))
		}
	}
	if _, _, err := ParseStatement("SELECT * FROM t WHERE a = '" + strings.Repeat("x", MaxQueryBytes) + "'"); err == nil {
		t.Errorf("oversized query should be rejected")
	}
}

func TestOnlyOneStatementPerRequest(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t; DROP TABLE t",
		"SELECT * FROM t; DELETE FROM t",
		"SELECT * FROM t WHERE a = 1; --",
	} {
		_, _, err := ParseStatement(q)
		if q == "SELECT * FROM t WHERE a = 1; --" {
			if err != nil {
				t.Errorf("a trailing comment is harmless: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q should be rejected: stacked statements are not in the grammar", q)
		}
	}
}

func TestLexerErrorsReadCleanly(t *testing.T) {
	_, _, err := ParseStatement("SELECT é FROM t")
	if err == nil {
		t.Fatal("a non-ASCII identifier is not in the grammar")
	}
	if got := err.Error(); got != `syntax error at byte 7: unexpected character 'é'` {
		t.Fatalf("message reads badly: %s", got)
	}
}

func TestStringEscaping(t *testing.T) {
	st := mustParse(t, "SELECT * FROM t WHERE a = 'it''s; DROP TABLE t; --'")
	cond := st.(Select).Plan.(Projection).Input.(Selection).Cond.(Binary)
	if got := cond.R.(Literal).V.S; got != "it's; DROP TABLE t; --" {
		t.Fatalf("got %q", got)
	}
}

func TestParamsNumberedInTextualOrder(t *testing.T) {
	st, n, err := ParseStatement("UPDATE t SET a = ?, b = ? WHERE c = ? AND d = ?")
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("counted %d params", n)
	}
	u := st.(Update)
	if u.Set[0].Value.(Param).N != 0 || u.Set[1].Value.(Param).N != 1 {
		t.Fatalf("SET params out of order: %+v", u.Set)
	}
	if !strings.Contains(u.Where.String(), "$3") || !strings.Contains(u.Where.String(), "$4") {
		t.Fatalf("WHERE params out of order: %s", u.Where)
	}
}

func TestAlgebraTranslation(t *testing.T) {
	st := mustParse(t, "SELECT o.id, c.name FROM orders o JOIN customers c ON o.customer_id = c.id WHERE c.id = ? ORDER BY o.id DESC LIMIT 3")
	want := "LIMIT[3](π[o.id, c.name](τ[o.id DESC](σ[(c.id = $1)]((ρ[o](orders) ⋈[(o.customer_id = c.id)] ρ[c](customers))))))"
	if got := st.(Select).Plan.String(); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestExplainTreeShowsDerivation(t *testing.T) {
	st := mustParse(t, "EXPLAIN SELECT a FROM t WHERE a = 1 OR b = 2")
	tree := st.(Explain).Tree.Pretty()
	for _, want := range []string{"select_stmt", "where_clause", "expr", "and_expr", "comparison", "keyword OR", "identifier a"} {
		if !strings.Contains(tree, want) {
			t.Errorf("tree lacks %q:\n%s", want, tree)
		}
	}
}

type mapEnv map[string]Value

func (m mapEnv) Column(r ColRef) (Value, error) { return m[r.Name], nil }
func (m mapEnv) Param(int) (Value, error)       { return Null, nil }

func evalStr(t *testing.T, src string, env mapEnv) (Value, error) {
	t.Helper()
	st := mustParse(t, "SELECT * FROM t WHERE "+src)
	return Eval(st.(Select).Plan.(Projection).Input.(Selection).Cond, env)
}

func TestThreeValuedLogicTruthTable(t *testing.T) {
	T, F, N := Bool(true), Bool(false), Null
	cases := []struct {
		src  string
		want Value
	}{
		{"p AND q", T}, {"p AND f", F}, {"p AND n", N},
		{"f AND p", F}, {"f AND n", F}, // false wins over unknown
		{"n AND p", N}, {"n AND f", F}, {"n AND n", N},
		{"p OR q", T}, {"p OR n", T}, // true wins over unknown
		{"f OR f", F}, {"f OR n", N}, {"n OR p", T}, {"n OR f", N}, {"n OR n", N},
		{"NOT p", F}, {"NOT f", T}, {"NOT n", N},
		{"n IS NULL", T}, {"p IS NULL", F}, {"n IS NOT NULL", F},
		{"n = n", N}, {"n <> n", N}, {"1 = n", N}, {"n < 1", N}, // NULL never equals NULL
		{"n + 1 = 2", N},
	}
	env := mapEnv{"p": T, "q": T, "f": F, "n": N}
	for _, c := range cases {
		got, err := evalStr(t, c.src, env)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
		} else if got != c.want {
			t.Errorf("%s: got %s, want %s", c.src, got, c.want)
		}
	}
}

func TestRuntimeErrorsNotSilentlyWrong(t *testing.T) {
	big := int64(math.MaxInt64)
	env := mapEnv{"big": Int(big), "small": Int(math.MinInt64), "s": Text("x"), "i": Int(2), "z": Int(0), "m": Int(-1)}
	for _, src := range []string{
		"big + 1 = 0", "small - 1 = 0", "big * 2 = 0", "small * m = 0", "small / m = 0", "- small = 0",
		"i / z = 0", "s + 1 = 0", "s = 1", "i AND i", "NOT i",
	} {
		if _, err := evalStr(t, src, env); err == nil {
			t.Errorf("%s should be an error, not a wrong answer", src)
		}
	}
}

func TestKeyEncodingSortsLikeValues(t *testing.T) {
	order := []int64{math.MinInt64, -100, -1, 0, 1, 99, 100, math.MaxInt64}
	for i := 1; i < len(order); i++ {
		a, b := EncodeKey([]Value{Int(order[i-1])}), EncodeKey([]Value{Int(order[i])})
		if a >= b {
			t.Errorf("%d should sort before %d: %q vs %q", order[i-1], order[i], a, b)
		}
	}
	if a, b := EncodeKey([]Value{Text("a")}), EncodeKey([]Value{Text("ab")}); a >= b {
		t.Errorf("text prefix should sort first")
	}
	// Composite keys must not collide across the column boundary.
	if EncodeKey([]Value{Text("ab"), Text("c")}) == EncodeKey([]Value{Text("a"), Text("bc")}) {
		t.Errorf("composite key collision")
	}
	if a, b := EncodeKey([]Value{Int(1), Int(9)}), EncodeKey([]Value{Int(2), Int(0)}); a >= b {
		t.Errorf("composite keys should sort by the first column first")
	}
}
