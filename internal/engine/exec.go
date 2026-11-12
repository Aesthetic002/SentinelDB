package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"sentineldb/internal/sql"
	"sentineldb/internal/txn"
)

// Backend is what the executor needs from the consensus layer.
type Backend interface {
	// LeaderEpoch returns the current term if this node is a leader that has caught up
	// on the whole committed log. It fails if this node is not the leader.
	LeaderEpoch(ctx context.Context) (uint64, error)
	// Commit replicates a command and returns once it has been applied locally. If the
	// epoch is no longer current it must refuse, as this transaction's locks are gone.
	Commit(ctx context.Context, epoch uint64, c *Command) (ApplyResult, error)
	// ReadBarrier confirms this node was still the leader at some point after the call
	// began, so that what it read under locks was the latest committed data.
	ReadBarrier(ctx context.Context, epoch uint64) error
}

// Result is what a statement returns.
type Result struct {
	Columns  []string
	Rows     [][]sql.Value
	Affected int
	Message  string
	// Stale marks a follower read that may lag the leader.
	Stale bool
	// Duplicate means a retried request was recognized and not applied twice.
	Duplicate bool
}

// Executor runs statements against a DB on behalf of many sessions.
type Executor struct {
	DB      *DB
	Locks   *txn.Manager
	Backend Backend
	// Recorder, when set, captures the real schedule for serializability checking.
	Recorder *txn.Recorder
	// DisableLocks removes locking entirely. It exists only so a test can show that
	// without 2PL the serializability check really does fail.
	DisableLocks bool
}

func tableRes(t string) string       { return "t/" + t }
func rowRes(t, key string) string    { return "r/" + t + "/" + key }
func tableItem(t string) string      { return t + ".*" }
func rowItem(t, key string) string   { return t + "/" + key }
func errUnknownTable(t string) error { return fmt.Errorf("table %s does not exist", t) }

// tx is one running transaction. Writes go to a private overlay and become visible to
// others only when the log entry carrying them commits.
type tx struct {
	ex       *Executor
	id       uint64
	epoch    uint64
	overlay  map[string]map[string]Row // table -> key -> row (nil means deleted)
	ops      []Op
	affected int
}

func (e *Executor) begin(ctx context.Context) (*tx, error) {
	epoch, err := e.Backend.LeaderEpoch(ctx)
	if err != nil {
		return nil, err
	}
	return &tx{ex: e, id: e.Locks.NewTxID(), epoch: epoch, overlay: map[string]map[string]Row{}}, nil
}

func (t *tx) lock(ctx context.Context, res string, m txn.Mode) error {
	if t.ex.DisableLocks {
		return nil
	}
	return t.ex.Locks.Acquire(ctx, t.epoch, t.id, res, m)
}

func (t *tx) lockTable(ctx context.Context, name string, m txn.Mode) error {
	return t.lock(ctx, tableRes(name), m)
}

func (t *tx) lockRow(ctx context.Context, name, key string, m txn.Mode) error {
	intent := txn.IS
	if m == txn.X {
		intent = txn.IX
	}
	if err := t.lockTable(ctx, name, intent); err != nil {
		return err
	}
	return t.lock(ctx, rowRes(name, key), m)
}

func (t *tx) record(write bool, item string) {
	if t.ex.Recorder != nil {
		t.ex.Recorder.Record(t.id, write, item)
	}
}

func (t *tx) schema(name string) (*Schema, error) {
	s := t.ex.DB.Schema(name)
	if s == nil {
		return nil, errUnknownTable(name)
	}
	return s, nil
}

// getRow reads one row through the overlay. The caller holds a lock on it.
func (t *tx) getRow(name, key string) (Row, bool) {
	if m, ok := t.overlay[name]; ok {
		if r, ok := m[key]; ok {
			return r, r != nil
		}
	}
	return t.ex.DB.get(name, key)
}

// scanRows reads a whole table through the overlay. The caller holds a table lock.
func (t *tx) scanRows(name string) []keyedRow {
	base := t.ex.DB.scan(name)
	over := t.overlay[name]
	if len(over) == 0 {
		return base
	}
	out := make([]keyedRow, 0, len(base)+len(over))
	for _, kr := range base {
		if _, shadowed := over[kr.key]; !shadowed {
			out = append(out, kr)
		}
	}
	for k, r := range over {
		if r != nil {
			out = append(out, keyedRow{k, r})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func (t *tx) put(name, key string, r Row) {
	m := t.overlay[name]
	if m == nil {
		m = map[string]Row{}
		t.overlay[name] = m
	}
	m[key] = r
	t.ops = append(t.ops, Op{Kind: OpPut, Table: name, Key: key, Row: r})
	t.record(true, rowItem(name, key))
}

func (t *tx) del(name, key string) {
	m := t.overlay[name]
	if m == nil {
		m = map[string]Row{}
		t.overlay[name] = m
	}
	m[key] = nil
	t.ops = append(t.ops, Op{Kind: OpDelete, Table: name, Key: key})
	t.record(true, rowItem(name, key))
	t.record(true, tableItem(name))
}

// ---------------------------------------------------------------------------
// Expression environment
// ---------------------------------------------------------------------------

type outCol struct {
	table, name string
	kind        sql.Kind
}

type rowEnv struct {
	cols   []outCol
	row    Row
	params []sql.Value
}

func (e *rowEnv) Param(n int) (sql.Value, error) {
	if n >= len(e.params) {
		return sql.Null, fmt.Errorf("query uses %d or more parameters but only %d were bound", n+1, len(e.params))
	}
	return e.params[n], nil
}

func (e *rowEnv) Column(ref sql.ColRef) (sql.Value, error) {
	i, err := resolve(e.cols, ref)
	if err != nil {
		return sql.Null, err
	}
	if e.row == nil {
		return sql.Null, nil
	}
	return e.row[i], nil
}

func resolve(cols []outCol, ref sql.ColRef) (int, error) {
	found := -1
	for i, c := range cols {
		if c.name == ref.Name && (ref.Table == "" || ref.Table == c.table) {
			if found >= 0 {
				return 0, fmt.Errorf("column %s is ambiguous; qualify it with a table name", ref.Name)
			}
			found = i
		}
	}
	if found < 0 {
		return 0, fmt.Errorf("column %s does not exist", ref)
	}
	return found, nil
}

// validate rejects bad column names up front, so that a typo errors even on an empty
// table, and before any lock is taken.
func validate(e sql.Expr, cols []outCol) error {
	if e == nil {
		return nil
	}
	for _, ref := range sql.ColumnsIn(e) {
		if _, err := resolve(cols, ref); err != nil {
			return err
		}
	}
	return nil
}

func passes(e sql.Expr, env *rowEnv) (bool, error) {
	if e == nil {
		return true, nil
	}
	v, err := sql.Eval(e, env)
	if err != nil {
		return false, err
	}
	if v.IsNull() {
		return false, nil // NULL is "unknown", and only true passes a filter
	}
	if v.K != sql.KindBool {
		return false, fmt.Errorf("condition must be BOOL, got %s", v.K)
	}
	return v.B, nil
}

func schemaCols(s *sql.Scan, sc *Schema) []outCol {
	cols := make([]outCol, len(sc.Columns))
	for i, c := range sc.Columns {
		cols[i] = outCol{s.Name(), c.Name, c.Type}
	}
	return cols
}

// ---------------------------------------------------------------------------
// Primary-key lookups
// ---------------------------------------------------------------------------

// pointKey detects a condition that pins every primary-key column to a constant. Such a
// statement needs to lock only one row, not the whole table.
func pointKey(where sql.Expr, sc *Schema, alias string, params []sql.Value) (string, bool) {
	bound := map[string]sql.Value{}
	for _, c := range sql.Conjuncts(where) {
		b, ok := c.(sql.Binary)
		if !ok || b.Op != "=" {
			continue
		}
		col, val := b.L, b.R
		if _, isCol := col.(sql.ColRef); !isCol {
			col, val = b.R, b.L
		}
		ref, isCol := col.(sql.ColRef)
		if !isCol || (ref.Table != "" && ref.Table != alias) {
			continue
		}
		var v sql.Value
		switch x := val.(type) {
		case sql.Literal:
			v = x.V
		case sql.Param:
			if x.N >= len(params) {
				return "", false
			}
			v = params[x.N]
		default:
			continue
		}
		bound[ref.Name] = v
	}
	vals := make([]sql.Value, len(sc.PK))
	for i, pk := range sc.PK {
		v, ok := bound[pk]
		if !ok || v.K != sc.Columns[sc.ColumnIndex(pk)].Type {
			// A type mismatch falls back to a scan, where it produces a proper error.
			return "", false
		}
		vals[i] = v
	}
	return sql.EncodeKey(vals), true
}

// ---------------------------------------------------------------------------
// SELECT: evaluates the relational algebra
// ---------------------------------------------------------------------------

type relation struct {
	cols []outCol
	rows []Row
}

func (t *tx) evalPlan(ctx context.Context, p sql.Plan, params []sql.Value) (relation, error) {
	switch x := p.(type) {
	case sql.Scan:
		sc, err := t.schema(x.Table)
		if err != nil {
			return relation{}, err
		}
		if err := t.lockTable(ctx, x.Table, txn.S); err != nil {
			return relation{}, err
		}
		t.record(false, tableItem(x.Table))
		kr := t.scanRows(x.Table)
		rows := make([]Row, len(kr))
		for i, r := range kr {
			rows[i] = r.row
			t.record(false, rowItem(x.Table, r.key))
		}
		return relation{schemaCols(&x, sc), rows}, nil

	case sql.Selection:
		// sigma directly over a scan, with the whole primary key pinned, is a point
		// lookup: lock and fetch one row instead of the whole table.
		if s, ok := x.Input.(sql.Scan); ok {
			sc, err := t.schema(s.Table)
			if err != nil {
				return relation{}, err
			}
			if err := validate(x.Cond, schemaCols(&s, sc)); err != nil {
				return relation{}, err
			}
			if key, ok := pointKey(x.Cond, sc, s.Name(), params); ok {
				return t.pointSelect(ctx, s, sc, key, x.Cond, params)
			}
		}
		in, err := t.evalPlan(ctx, x.Input, params)
		if err != nil {
			return relation{}, err
		}
		if err := validate(x.Cond, in.cols); err != nil {
			return relation{}, err
		}
		env := &rowEnv{cols: in.cols, params: params}
		out := relation{cols: in.cols}
		for _, r := range in.rows {
			env.row = r
			ok, err := passes(x.Cond, env)
			if err != nil {
				return relation{}, err
			}
			if ok {
				out.rows = append(out.rows, r)
			}
		}
		return out, nil

	case sql.Join:
		return t.evalJoin(ctx, x, params)

	case sql.Sort:
		in, err := t.evalPlan(ctx, x.Input, params)
		if err != nil {
			return relation{}, err
		}
		i, err := resolve(in.cols, x.Key)
		if err != nil {
			return relation{}, err
		}
		var cmpErr error
		sort.SliceStable(in.rows, func(a, b int) bool {
			va, vb := in.rows[a][i], in.rows[b][i]
			if va.IsNull() || vb.IsNull() { // NULLs sort first in ascending order
				less := va.IsNull() && !vb.IsNull()
				if x.Desc {
					return vb.IsNull() && !va.IsNull()
				}
				return less
			}
			c, err := sql.Compare(va, vb)
			if err != nil {
				cmpErr = err
			}
			if x.Desc {
				return c > 0
			}
			return c < 0
		})
		return in, cmpErr

	case sql.Projection:
		in, err := t.evalPlan(ctx, x.Input, params)
		if err != nil {
			return relation{}, err
		}
		if x.Cols == nil {
			return in, nil
		}
		idx := make([]int, len(x.Cols))
		cols := make([]outCol, len(x.Cols))
		for i, c := range x.Cols {
			j, err := resolve(in.cols, c)
			if err != nil {
				return relation{}, err
			}
			idx[i], cols[i] = j, in.cols[j]
		}
		out := relation{cols: cols, rows: make([]Row, len(in.rows))}
		for i, r := range in.rows {
			nr := make(Row, len(idx))
			for k, j := range idx {
				nr[k] = r[j]
			}
			out.rows[i] = nr
		}
		return out, nil

	case sql.Limit:
		in, err := t.evalPlan(ctx, x.Input, params)
		if err != nil {
			return relation{}, err
		}
		if int64(len(in.rows)) > x.N {
			in.rows = in.rows[:x.N]
		}
		return in, nil
	}
	return relation{}, fmt.Errorf("internal error: unknown plan node %T", p)
}

func (t *tx) pointSelect(ctx context.Context, s sql.Scan, sc *Schema, key string, cond sql.Expr, params []sql.Value) (relation, error) {
	if err := t.lockRow(ctx, s.Table, key, txn.S); err != nil {
		return relation{}, err
	}
	t.record(false, rowItem(s.Table, key))
	out := relation{cols: schemaCols(&s, sc)}
	r, ok := t.getRow(s.Table, key)
	if !ok {
		return out, nil
	}
	pass, err := passes(cond, &rowEnv{cols: out.cols, row: r, params: params})
	if err != nil {
		return relation{}, err
	}
	if pass {
		out.rows = append(out.rows, r)
	}
	return out, nil
}

// evalJoin uses a hash join for an equality between the two sides and a nested loop
// otherwise. Both implement the same theta join, and a test checks they agree.
func (t *tx) evalJoin(ctx context.Context, x sql.Join, params []sql.Value) (relation, error) {
	l, err := t.evalPlan(ctx, x.Left, params)
	if err != nil {
		return relation{}, err
	}
	r, err := t.evalPlan(ctx, x.Right, params)
	if err != nil {
		return relation{}, err
	}
	cols := append(append([]outCol(nil), l.cols...), r.cols...)
	if err := validate(x.Cond, cols); err != nil {
		return relation{}, err
	}
	out := relation{cols: cols}
	env := &rowEnv{cols: cols, params: params}
	emit := func(a, b Row) (bool, Row, error) {
		joined := make(Row, 0, len(a)+len(b))
		joined = append(append(joined, a...), b...)
		env.row = joined
		ok, err := passes(x.Cond, env)
		return ok, joined, err
	}

	if li, ri, ok := equiJoinColumns(x.Cond, l.cols, r.cols); ok {
		build := make(map[string][]Row, len(r.rows))
		for _, b := range r.rows {
			if v := b[ri]; !v.IsNull() { // NULL never equals anything
				k := sql.EncodeKey([]sql.Value{v})
				build[k] = append(build[k], b)
			}
		}
		for _, a := range l.rows {
			if a[li].IsNull() {
				continue
			}
			for _, b := range build[sql.EncodeKey([]sql.Value{a[li]})] {
				keep, joined, err := emit(a, b)
				if err != nil {
					return relation{}, err
				}
				if keep {
					out.rows = append(out.rows, joined)
				}
			}
		}
		return out, nil
	}

	for _, a := range l.rows {
		for _, b := range r.rows {
			keep, joined, err := emit(a, b)
			if err != nil {
				return relation{}, err
			}
			if keep {
				out.rows = append(out.rows, joined)
			}
		}
	}
	return out, nil
}

func equiJoinColumns(cond sql.Expr, l, r []outCol) (int, int, bool) {
	b, ok := cond.(sql.Binary)
	if !ok || b.Op != "=" {
		return 0, 0, false
	}
	a, ok1 := b.L.(sql.ColRef)
	c, ok2 := b.R.(sql.ColRef)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	// Comparing different types must reach the evaluator and report an error rather than
	// quietly producing an empty hash join, so a mismatch falls back to the nested loop.
	ll, e1 := resolve(l, a)
	rr, e2 := resolve(r, c)
	if e1 == nil && e2 == nil {
		return ll, rr, l[ll].kind == r[rr].kind
	}
	ll, e1 = resolve(l, c)
	rr, e2 = resolve(r, a)
	if e1 == nil && e2 == nil {
		return ll, rr, l[ll].kind == r[rr].kind
	}
	return 0, 0, false
}

func columnNames(cols []outCol) []string {
	dup := map[string]int{}
	for _, c := range cols {
		dup[c.name]++
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		if dup[c.name] > 1 {
			out[i] = c.table + "." + c.name
		} else {
			out[i] = c.name
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// checkRow enforces types and NOT NULL. It also makes primary-key columns implicitly
// NOT NULL, since a key that can be NULL identifies nothing.
func checkRow(sc *Schema, r Row) error {
	for i, c := range sc.Columns {
		v := r[i]
		if v.IsNull() {
			isPK := false
			for _, pk := range sc.PK {
				isPK = isPK || pk == c.Name
			}
			if c.NotNull || isPK {
				return fmt.Errorf("column %s.%s cannot be NULL", sc.Name, c.Name)
			}
			continue
		}
		if v.K != c.Type {
			return fmt.Errorf("column %s.%s is %s, got %s value %s", sc.Name, c.Name, c.Type, v.K, v)
		}
	}
	return nil
}

// checkForeignKeys verifies each parent exists, holding an S lock so that it cannot be
// deleted before this transaction commits.
func (t *tx) checkForeignKeys(ctx context.Context, sc *Schema, r Row) error {
	for _, fk := range sc.Foreign {
		v := r[sc.ColumnIndex(fk.Column)]
		if v.IsNull() {
			continue
		}
		key := sql.EncodeKey([]sql.Value{v})
		if err := t.lockRow(ctx, fk.RefTable, key, txn.S); err != nil {
			return err
		}
		t.record(false, rowItem(fk.RefTable, key))
		if _, ok := t.getRow(fk.RefTable, key); !ok {
			return fmt.Errorf("foreign key violation: %s.%s = %s has no matching row in %s",
				sc.Name, fk.Column, v, fk.RefTable)
		}
	}
	return nil
}

func (t *tx) execInsert(ctx context.Context, st sql.Insert, params []sql.Value) (int, error) {
	sc, err := t.schema(st.Table)
	if err != nil {
		return 0, err
	}
	positions := make([]int, 0, len(sc.Columns))
	if st.Columns == nil {
		for i := range sc.Columns {
			positions = append(positions, i)
		}
	} else {
		seen := map[string]bool{}
		for _, c := range st.Columns {
			i := sc.ColumnIndex(c)
			if i < 0 {
				return 0, fmt.Errorf("column %s.%s does not exist", st.Table, c)
			}
			if seen[c] {
				return 0, fmt.Errorf("column %s is listed twice", c)
			}
			seen[c] = true
			positions = append(positions, i)
		}
	}
	env := &rowEnv{params: params}
	for _, exprs := range st.Rows {
		if len(exprs) != len(positions) {
			return 0, fmt.Errorf("INSERT has %d values for %d columns", len(exprs), len(positions))
		}
		r := make(Row, len(sc.Columns))
		for i, e := range exprs {
			for _, ref := range sql.ColumnsIn(e) {
				return 0, fmt.Errorf("INSERT values cannot refer to column %s", ref)
			}
			v, err := sql.Eval(e, env)
			if err != nil {
				return 0, err
			}
			r[positions[i]] = v
		}
		if err := checkRow(sc, r); err != nil {
			return 0, err
		}
		key := sc.Key(r)
		if err := t.lockRow(ctx, st.Table, key, txn.X); err != nil {
			return 0, err
		}
		if _, exists := t.getRow(st.Table, key); exists {
			return 0, fmt.Errorf("duplicate primary key in %s", st.Table)
		}
		if err := t.checkForeignKeys(ctx, sc, r); err != nil {
			return 0, err
		}
		t.put(st.Table, key, r)
		t.record(true, tableItem(st.Table))
	}
	return len(st.Rows), nil
}

// targetRows locks and returns what an UPDATE or DELETE touches: X on one row for a
// primary-key lookup, otherwise X on the whole table.
func (t *tx) targetRows(ctx context.Context, name string, sc *Schema, where sql.Expr, params []sql.Value) ([]keyedRow, error) {
	cols := schemaCols(&sql.Scan{Table: name}, sc)
	if err := validate(where, cols); err != nil {
		return nil, err
	}
	var candidates []keyedRow
	if key, ok := pointKey(where, sc, name, params); ok && where != nil {
		if err := t.lockRow(ctx, name, key, txn.X); err != nil {
			return nil, err
		}
		t.record(false, rowItem(name, key))
		if r, ok := t.getRow(name, key); ok {
			candidates = []keyedRow{{key, r}}
		}
	} else {
		if err := t.lockTable(ctx, name, txn.X); err != nil {
			return nil, err
		}
		t.record(false, tableItem(name))
		candidates = t.scanRows(name)
		for _, c := range candidates {
			t.record(false, rowItem(name, c.key))
		}
	}
	env := &rowEnv{cols: cols, params: params}
	out := candidates[:0:0]
	for _, c := range candidates {
		env.row = c.row
		ok, err := passes(where, env)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (t *tx) execUpdate(ctx context.Context, st sql.Update, params []sql.Value) (int, error) {
	sc, err := t.schema(st.Table)
	if err != nil {
		return 0, err
	}
	cols := schemaCols(&sql.Scan{Table: st.Table}, sc)
	positions := make([]int, len(st.Set))
	touchesFK := map[string]bool{}
	for i, a := range st.Set {
		idx := sc.ColumnIndex(a.Column)
		if idx < 0 {
			return 0, fmt.Errorf("column %s.%s does not exist", st.Table, a.Column)
		}
		for _, pk := range sc.PK {
			if pk == a.Column {
				return 0, fmt.Errorf("column %s.%s is part of the primary key and cannot be updated", st.Table, a.Column)
			}
		}
		if err := validate(a.Value, cols); err != nil {
			return 0, err
		}
		positions[i] = idx
		touchesFK[a.Column] = true
	}
	targets, err := t.targetRows(ctx, st.Table, sc, st.Where, params)
	if err != nil {
		return 0, err
	}
	env := &rowEnv{cols: cols, params: params}
	for _, target := range targets {
		env.row = target.row
		nr := append(Row(nil), target.row...)
		for i, a := range st.Set {
			v, err := sql.Eval(a.Value, env)
			if err != nil {
				return 0, err
			}
			nr[positions[i]] = v
		}
		if err := checkRow(sc, nr); err != nil {
			return 0, err
		}
		for _, fk := range sc.Foreign {
			if touchesFK[fk.Column] {
				if err := t.checkForeignKeys(ctx, sc, nr); err != nil {
					return 0, err
				}
				break
			}
		}
		t.put(st.Table, target.key, nr)
	}
	return len(targets), nil
}

func (t *tx) execDelete(ctx context.Context, st sql.Delete, params []sql.Value) (int, error) {
	sc, err := t.schema(st.Table)
	if err != nil {
		return 0, err
	}
	targets, err := t.targetRows(ctx, st.Table, sc, st.Where, params)
	if err != nil {
		return 0, err
	}
	children := t.ex.DB.childrenOf(st.Table)
	for _, target := range targets {
		if len(children) > 0 {
			if err := t.ensureNoChildren(ctx, sc, target, children); err != nil {
				return 0, err
			}
		}
		t.del(st.Table, target.key)
	}
	return len(targets), nil
}

// ensureNoChildren enforces referential integrity on delete, by refusal (RESTRICT).
// S on the child table blocks any concurrent insert of a new child.
func (t *tx) ensureNoChildren(ctx context.Context, parent *Schema, target keyedRow, children []*Schema) error {
	pkVal := target.row[parent.ColumnIndex(parent.PK[0])]
	for _, child := range children {
		for _, fk := range child.Foreign {
			if fk.RefTable != parent.Name {
				continue
			}
			if err := t.lockTable(ctx, child.Name, txn.S); err != nil {
				return err
			}
			t.record(false, tableItem(child.Name))
			i := child.ColumnIndex(fk.Column)
			for _, kr := range t.scanRows(child.Name) {
				if v := kr.row[i]; !v.IsNull() {
					if c, err := sql.Compare(v, pkVal); err == nil && c == 0 {
						return fmt.Errorf("foreign key violation: %s row is still referenced by %s.%s",
							parent.Name, child.Name, fk.Column)
					}
				}
			}
		}
	}
	return nil
}

func (e *Executor) execCreate(ctx context.Context, st sql.CreateTable, reqID string) (*Result, error) {
	t, err := e.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer e.finish(t)
	if dup := e.alreadyApplied(reqID); dup != nil {
		dup.Message = "table " + st.Name + " created"
		return dup, nil
	}
	if err := t.lockTable(ctx, st.Name, txn.X); err != nil {
		return nil, err
	}
	if e.DB.Schema(st.Name) != nil {
		return nil, fmt.Errorf("table %s already exists", st.Name)
	}
	sc := &Schema{Name: st.Name, PK: st.PrimaryKey}
	for _, c := range st.Columns {
		sc.Columns = append(sc.Columns, Column{Name: c.Name, Type: c.Type, NotNull: c.NotNull})
	}
	for _, pk := range st.PrimaryKey {
		if sc.ColumnIndex(pk) < 0 {
			return nil, fmt.Errorf("primary key column %s is not defined", pk)
		}
	}
	for _, fk := range st.Foreign {
		ci := sc.ColumnIndex(fk.Column)
		if ci < 0 {
			return nil, fmt.Errorf("foreign key column %s is not defined", fk.Column)
		}
		parent := sc // a table may reference itself
		if fk.RefTable != st.Name {
			parent = e.DB.Schema(fk.RefTable)
		}
		if parent == nil {
			return nil, fmt.Errorf("foreign key refers to table %s, which does not exist", fk.RefTable)
		}
		if len(parent.PK) != 1 || parent.PK[0] != fk.RefColumn {
			return nil, fmt.Errorf("foreign key must reference the single-column primary key of %s", fk.RefTable)
		}
		if parent.Columns[parent.ColumnIndex(fk.RefColumn)].Type != sc.Columns[ci].Type {
			return nil, fmt.Errorf("foreign key %s has a different type from %s.%s", fk.Column, fk.RefTable, fk.RefColumn)
		}
		sc.Foreign = append(sc.Foreign, ForeignKey{Column: fk.Column, RefTable: fk.RefTable})
	}
	cmd := &Command{RequestID: reqID, Ops: []Op{{Kind: OpCreateTable, Schema: sc}}}
	res, err := e.Backend.Commit(ctx, t.epoch, cmd)
	if err != nil {
		return nil, err
	}
	if res.Err != "" {
		return nil, errors.New(res.Err)
	}
	return &Result{Message: "table " + st.Name + " created", Duplicate: res.Duplicate}, nil
}

func (e *Executor) finish(t *tx) {
	if !e.DisableLocks {
		e.Locks.ReleaseAll(t.id)
	}
}

// commit ends a transaction. A transaction that wrote replicates its whole write set as
// one log entry, so it is atomic across a crash. A read-only one still confirms
// leadership: otherwise a deposed leader could return data that is no longer current.
func (t *tx) commit(ctx context.Context, reqID string) (ApplyResult, error) {
	if len(t.ops) == 0 {
		return ApplyResult{}, t.ex.Backend.ReadBarrier(ctx, t.epoch)
	}
	r, err := t.ex.Backend.Commit(ctx, t.epoch, &Command{RequestID: reqID, Ops: t.ops, Affected: t.affected})
	if err == nil && r.Err != "" {
		err = errors.New(r.Err)
	}
	return r, err
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// Session is one client connection's state: at most one open transaction.
type Session struct {
	ex *Executor
	tx *tx
	// autoRetries bounds how often an autocommit statement retries after wait-die.
	autoRetries int
}

// NewSession starts a session.
func (e *Executor) NewSession() *Session { return &Session{ex: e, autoRetries: 64} }

// InTransaction reports whether an explicit transaction is open.
func (s *Session) InTransaction() bool { return s.tx != nil }

// Abort rolls back any open transaction, for when a connection closes.
func (s *Session) Abort() {
	if s.tx != nil {
		s.ex.finish(s.tx)
		s.tx = nil
	}
}

// Execute runs one SQL statement. Text and parameters stay separate all the way down.
func (s *Session) Execute(ctx context.Context, text string, params []sql.Value, reqID string) (*Result, error) {
	st, want, err := sql.ParseStatement(text)
	if err != nil {
		// A syntax error inside a transaction leaves it open: nothing has been touched
		// and the client can simply send a corrected statement.
		return nil, err
	}
	if want != len(params) {
		return nil, fmt.Errorf("statement has %d parameter(s) but %d value(s) were bound", want, len(params))
	}

	switch x := st.(type) {
	case sql.Explain:
		return explain(x), nil
	case sql.Begin:
		if s.tx != nil {
			return nil, errors.New("a transaction is already open")
		}
		t, err := s.ex.begin(ctx)
		if err != nil {
			return nil, err
		}
		s.tx = t
		return &Result{Message: "transaction started"}, nil
	case sql.Commit:
		if s.tx == nil {
			return nil, errors.New("no transaction is open")
		}
		t := s.tx
		s.tx = nil
		defer s.ex.finish(t)
		r, err := t.commit(ctx, reqID)
		if err != nil {
			return nil, err
		}
		if s.ex.Recorder != nil {
			s.ex.Recorder.Committed(t.id)
		}
		return &Result{Message: "transaction committed", Affected: t.affected, Duplicate: r.Duplicate}, nil
	case sql.Rollback:
		if s.tx == nil {
			return nil, errors.New("no transaction is open")
		}
		s.Abort()
		return &Result{Message: "transaction rolled back"}, nil
	case sql.CreateTable:
		if s.tx != nil {
			return nil, errors.New("CREATE TABLE is not allowed inside a transaction")
		}
		return s.ex.execCreate(ctx, x, reqID)
	}

	if s.tx != nil {
		res, err := s.tx.exec(ctx, st, params)
		if err != nil {
			// Any failure aborts the transaction. A partly applied statement would
			// otherwise leave its effects in the overlay and be committed later.
			s.Abort()
			return nil, fmt.Errorf("transaction aborted: %w", err)
		}
		return res, nil
	}
	return s.autocommit(ctx, st, params, reqID)
}

// autocommit wraps a lone statement in its own transaction. It may safely retry after
// wait-die because nothing has been proposed at that point, and the transaction keeps
// its ID so that it ages and eventually wins.
func (s *Session) autocommit(ctx context.Context, st sql.Statement, params []sql.Value, reqID string) (*Result, error) {
	t, err := s.ex.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ex.finish(t)
	// Reaching here means this node has applied the whole committed log (LeaderEpoch
	// guarantees it), so a retried request that already succeeded is visible now.
	if dup := s.ex.alreadyApplied(reqID); dup != nil {
		return dup, nil
	}
	for attempt := 0; ; attempt++ {
		res, err := t.exec(ctx, st, params)
		if err == nil {
			var ar ApplyResult
			if ar, err = t.commit(ctx, reqID); err != nil {
				return nil, err
			}
			if s.ex.Recorder != nil {
				s.ex.Recorder.Committed(t.id)
			}
			if ar.Duplicate {
				res.Affected = ar.Affected
				res.Duplicate = true
			}
			return res, nil
		}
		// The first attempt of a retried request can still be in flight when the retry
		// starts. The retry then waits on its locks and fails on a constraint it itself
		// satisfied, such as a duplicate key. That is success, not an error.
		if dup := s.ex.alreadyApplied(reqID); dup != nil {
			return dup, nil
		}
		if !errors.Is(err, txn.ErrDie) || attempt >= s.autoRetries {
			return nil, err
		}
		s.ex.finish(t)
		if s.ex.Recorder != nil {
			s.ex.Recorder.Forget(t.id) // the failed attempt's actions never took effect
		}
		t.overlay = map[string]map[string]Row{}
		t.ops = nil
		t.affected = 0
		if err := sleepBackoff(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

func (e *Executor) alreadyApplied(reqID string) *Result {
	if reqID == "" {
		return nil
	}
	if n, ok := e.DB.seen(reqID); ok {
		return &Result{Affected: n, Duplicate: true}
	}
	return nil
}

// sleepBackoff waits a little before retrying, with jitter so that contending
// transactions spread out instead of colliding again in lockstep.
func sleepBackoff(ctx context.Context, attempt int) error {
	limit := time.Millisecond << min(attempt, 6)
	select {
	case <-time.After(time.Duration(rand.Int64N(int64(limit)) + 1)):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *tx) exec(ctx context.Context, st sql.Statement, params []sql.Value) (*Result, error) {
	switch x := st.(type) {
	case sql.Select:
		rel, err := t.evalPlan(ctx, x.Plan, params)
		if err != nil {
			return nil, err
		}
		return &Result{Columns: columnNames(rel.cols), Rows: rel.rows}, nil
	case sql.Insert:
		n, err := t.execInsert(ctx, x, params)
		t.affected += n
		return &Result{Affected: n}, err
	case sql.Update:
		n, err := t.execUpdate(ctx, x, params)
		t.affected += n
		return &Result{Affected: n}, err
	case sql.Delete:
		n, err := t.execDelete(ctx, x, params)
		t.affected += n
		return &Result{Affected: n}, err
	}
	return nil, fmt.Errorf("internal error: cannot execute %T", st)
}

func explain(x sql.Explain) *Result {
	var b strings.Builder
	b.WriteString("Parse tree:\n")
	b.WriteString(x.Tree.Pretty())
	b.WriteString("\nRelational algebra:\n  ")
	b.WriteString(sql.AlgebraString(x.Inner))
	return &Result{Message: b.String()}
}

// ExecuteStale serves a SELECT from this node's local copy with no locks and no
// leadership check. It may be behind the leader, in exchange for working on any node
// and adding no consensus round trip. The result is marked Stale so that the choice
// remains visible to the caller.
func (e *Executor) ExecuteStale(ctx context.Context, text string, params []sql.Value) (*Result, error) {
	st, want, err := sql.ParseStatement(text)
	if err != nil {
		return nil, err
	}
	if want != len(params) {
		return nil, fmt.Errorf("statement has %d parameter(s) but %d value(s) were bound", want, len(params))
	}
	sel, ok := st.(sql.Select)
	if !ok {
		return nil, errors.New("stale reads must be SELECT statements")
	}
	reader := &Executor{DB: e.DB, DisableLocks: true}
	t := &tx{ex: reader, overlay: map[string]map[string]Row{}}
	rel, err := t.evalPlan(ctx, sel.Plan, params)
	if err != nil {
		return nil, err
	}
	return &Result{Columns: columnNames(rel.cols), Rows: rel.rows, Stale: true}, nil
}
