package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sentineldb/internal/sql"
	"sentineldb/internal/txn"
)

// localBackend applies commands directly, standing in for Raft.
type localBackend struct {
	db       *DB
	mu       sync.Mutex
	commits  int
	barriers atomic.Int64
	epoch    uint64
	failNext error
}

func (b *localBackend) LeaderEpoch(context.Context) (uint64, error) { return b.epoch, nil }
func (b *localBackend) ReadBarrier(context.Context, uint64) error {
	b.barriers.Add(1)
	return nil
}
func (b *localBackend) Commit(_ context.Context, epoch uint64, c *Command) (ApplyResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failNext != nil {
		err := b.failNext
		b.failNext = nil
		return ApplyResult{}, err
	}
	if epoch != b.epoch {
		return ApplyResult{}, errors.New("stale epoch")
	}
	b.commits++
	return b.db.Apply(c), nil
}

func newExec() (*Executor, *localBackend) {
	db := NewDB()
	be := &localBackend{db: db}
	return &Executor{DB: db, Locks: txn.NewManager(), Backend: be}, be
}

func run(t *testing.T, s *Session, q string, params ...sql.Value) *Result {
	t.Helper()
	r, err := s.Execute(context.Background(), q, params, "")
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return r
}

func runErr(t *testing.T, s *Session, q string, want string, params ...sql.Value) {
	t.Helper()
	_, err := s.Execute(context.Background(), q, params, "")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: want error containing %q, got %v", q, want, err)
	}
}

func rowsOf(r *Result) string {
	var lines []string
	for _, row := range r.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = v.String()
		}
		lines = append(lines, strings.Join(cells, ","))
	}
	return strings.Join(lines, ";")
}

func setupShop(t *testing.T, s *Session) {
	t.Helper()
	for _, q := range []string{
		"CREATE TABLE customers (id INT PRIMARY KEY, name TEXT NOT NULL, address TEXT)",
		"CREATE TABLE products (id INT PRIMARY KEY, name TEXT NOT NULL, price_cents INT NOT NULL)",
		"CREATE TABLE orders (id INT PRIMARY KEY, customer_id INT NOT NULL REFERENCES customers (id), status TEXT NOT NULL)",
		`CREATE TABLE order_items (order_id INT, product_id INT, quantity INT NOT NULL,
			PRIMARY KEY (order_id, product_id),
			FOREIGN KEY (order_id) REFERENCES orders (id),
			FOREIGN KEY (product_id) REFERENCES products (id))`,
		"INSERT INTO customers VALUES (1, 'Asha', 'Bengaluru'), (2, 'Ravi', NULL), (42, 'Meera', 'Mysuru')",
		"INSERT INTO products VALUES (10, 'Pen', 500), (11, 'Book', 25000)",
		"INSERT INTO orders VALUES (100, 42, 'open'), (101, 42, 'shipped'), (102, 1, 'open')",
		"INSERT INTO order_items VALUES (100, 10, 3), (100, 11, 1), (102, 10, 1)",
	} {
		run(t, s, q)
	}
}

func TestSelectProjectFilterSortLimit(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	setupShop(t, s)
	cases := []struct{ q, want string }{
		{"SELECT id FROM customers", "1;2;42"},
		{"SELECT name FROM customers WHERE id = 42", "'Meera'"},
		{"SELECT * FROM customers WHERE id = 2", "2,'Ravi',NULL"},
		{"SELECT id FROM customers WHERE address IS NULL", "2"},
		{"SELECT id FROM customers WHERE address = 'Mysuru' OR id = 1", "1;42"},
		{"SELECT id FROM customers WHERE id > 1 AND NOT id = 42", "2"},
		{"SELECT id FROM customers ORDER BY id DESC", "42;2;1"},
		{"SELECT id FROM customers ORDER BY address", "2;1;42"}, // NULL first
		{"SELECT id FROM customers ORDER BY id DESC LIMIT 2", "42;2"},
		{"SELECT id FROM customers LIMIT 0", ""},
		{"SELECT id FROM customers WHERE id = 999", ""},
		// NULL is unknown, so neither branch of the comparison matches the NULL row.
		{"SELECT id FROM customers WHERE address <> 'x' ORDER BY id", "1;42"},
		{"SELECT id FROM customers WHERE NOT address <> 'x'", ""},
		{"SELECT price_cents FROM products WHERE price_cents + 1 = 501", "500"},
	}
	for _, c := range cases {
		if got := rowsOf(run(t, s, c.q)); got != c.want {
			t.Errorf("%s\n  got  %s\n  want %s", c.q, got, c.want)
		}
	}
}

func TestJoins(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	setupShop(t, s)
	cases := []struct{ q, want string }{
		{"SELECT o.id, c.name FROM orders o JOIN customers c ON o.customer_id = c.id WHERE c.id = 42 ORDER BY o.id",
			"100,'Meera';101,'Meera'"},
		{"SELECT c.name, p.name, i.quantity FROM customers c JOIN orders o ON o.customer_id = c.id " +
			"JOIN order_items i ON i.order_id = o.id JOIN products p ON p.id = i.product_id ORDER BY i.quantity DESC LIMIT 1",
			"'Meera','Pen',3"},
		{"SELECT o.id FROM orders o JOIN customers c ON c.id = o.customer_id AND c.name = 'Asha'", "102"},
	}
	for _, c := range cases {
		if got := rowsOf(run(t, s, c.q)); got != c.want {
			t.Errorf("%s\n  got  %s\n  want %s", c.q, got, c.want)
		}
	}
	runErr(t, s, "SELECT id FROM orders o JOIN customers c ON o.customer_id = c.id", "ambiguous")
	runErr(t, s, "SELECT * FROM orders o JOIN orders o ON o.id = o.id", "twice")
	runErr(t, s, "SELECT o.id FROM orders o JOIN customers c ON o.id = c.name", "cannot compare")
}

// The hash join must give exactly what the definition of a theta join gives.
func TestHashJoinMatchesNestedLoop(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	run(t, s, "CREATE TABLE a (id INT PRIMARY KEY, k INT)")
	run(t, s, "CREATE TABLE b (id INT PRIMARY KEY, k INT)")
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 60; i++ {
		var k sql.Value = sql.Int(int64(r.Intn(8)))
		if r.Intn(5) == 0 {
			k = sql.Null
		}
		run(t, s, "INSERT INTO a VALUES (?, ?)", sql.Int(int64(i)), k)
		k = sql.Int(int64(r.Intn(8)))
		if r.Intn(5) == 0 {
			k = sql.Null
		}
		run(t, s, "INSERT INTO b VALUES (?, ?)", sql.Int(int64(i)), k)
	}
	hash := run(t, s, "SELECT a.id, b.id FROM a JOIN b ON a.k = b.k")
	// "AND TRUE" stops the equality being recognized, which forces the nested loop.
	loop := run(t, s, "SELECT a.id, b.id FROM a JOIN b ON a.k = b.k AND TRUE")
	if rowsOf(hash) != rowsOf(loop) || len(hash.Rows) == 0 {
		t.Fatalf("hash join and nested loop disagree:\n%s\n%s", rowsOf(hash), rowsOf(loop))
	}
}

func TestConstraints(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	setupShop(t, s)
	runErr(t, s, "INSERT INTO customers VALUES (1, 'Dup', NULL)", "duplicate primary key")
	runErr(t, s, "INSERT INTO customers VALUES (NULL, 'x', NULL)", "cannot be NULL")
	runErr(t, s, "INSERT INTO customers VALUES (5, NULL, NULL)", "cannot be NULL")
	runErr(t, s, "INSERT INTO customers VALUES ('five', 'x', NULL)", "is INT")
	runErr(t, s, "INSERT INTO customers VALUES (5)", "1 values for 3 columns")
	runErr(t, s, "INSERT INTO customers (nope) VALUES (5)", "does not exist")
	runErr(t, s, "INSERT INTO customers (id, id, name) VALUES (5, 5, 'x')", "listed twice")
	runErr(t, s, "INSERT INTO customers VALUES (id, 'x', NULL)", "cannot refer to column")
	runErr(t, s, "INSERT INTO orders VALUES (200, 999, 'open')", "foreign key violation")
	runErr(t, s, "UPDATE orders SET customer_id = 999 WHERE id = 100", "foreign key violation")
	runErr(t, s, "UPDATE customers SET id = 7 WHERE id = 1", "part of the primary key")
	runErr(t, s, "UPDATE customers SET name = NULL WHERE id = 1", "cannot be NULL")
	runErr(t, s, "DELETE FROM customers WHERE id = 42", "still referenced")
	runErr(t, s, "SELECT nope FROM customers", "does not exist")
	runErr(t, s, "SELECT * FROM customers WHERE nope = 1", "does not exist")
	runErr(t, s, "SELECT * FROM nope", "does not exist")
	runErr(t, s, "SELECT * FROM customers WHERE name", "must be BOOL")
	runErr(t, s, "CREATE TABLE customers (id INT PRIMARY KEY)", "already exists")
	runErr(t, s, "CREATE TABLE x (id INT PRIMARY KEY, p INT REFERENCES nope (id))", "does not exist")
	runErr(t, s, "CREATE TABLE x (id INT PRIMARY KEY, p INT REFERENCES customers (name))", "primary key")
	runErr(t, s, "CREATE TABLE x (id INT PRIMARY KEY, p TEXT REFERENCES customers (id))", "different type")
	runErr(t, s, "CREATE TABLE x (id INT, PRIMARY KEY (nope))", "not defined")

	// A child can go once it is the parent that no longer has references.
	run(t, s, "DELETE FROM order_items WHERE order_id = 102 AND product_id = 10")
	run(t, s, "DELETE FROM orders WHERE id = 102")
	run(t, s, "DELETE FROM customers WHERE id = 1")
	// A self-reference.
	run(t, s, "CREATE TABLE emp (id INT PRIMARY KEY, boss INT REFERENCES emp (id))")
	run(t, s, "INSERT INTO emp VALUES (1, NULL), (2, 1)")
	runErr(t, s, "INSERT INTO emp VALUES (3, 99)", "foreign key violation")
	runErr(t, s, "DELETE FROM emp WHERE id = 1", "still referenced")
}

func TestUpdateAndDeleteReportCounts(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	setupShop(t, s)
	if n := run(t, s, "UPDATE products SET price_cents = price_cents * 2").Affected; n != 2 {
		t.Errorf("update affected %d", n)
	}
	if got := rowsOf(run(t, s, "SELECT price_cents FROM products")); got != "1000;50000" {
		t.Errorf("got %s", got)
	}
	if n := run(t, s, "UPDATE products SET price_cents = 1 WHERE id = 999").Affected; n != 0 {
		t.Errorf("update of nothing affected %d", n)
	}
	if n := run(t, s, "DELETE FROM order_items").Affected; n != 3 {
		t.Errorf("delete affected %d", n)
	}
}

// Parameters are data. These are classic injection payloads, and none of them may do
// anything but be compared as strings.
func TestParametersCannotChangeQueryStructure(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	setupShop(t, s)
	for _, payload := range []string{
		"' OR '1'='1", "x'; DROP TABLE customers; --", "' UNION SELECT * FROM products --",
		"Asha' --", "\\'; DELETE FROM customers; --",
	} {
		r := run(t, s, "SELECT id FROM customers WHERE name = ?", sql.Text(payload))
		if len(r.Rows) != 0 {
			t.Errorf("payload %q matched %d rows", payload, len(r.Rows))
		}
		run(t, s, "INSERT INTO products VALUES (?, ?, 1)", sql.Int(int64(1000+len(payload))), sql.Text(payload))
		r = run(t, s, "SELECT name FROM products WHERE name = ?", sql.Text(payload))
		if len(r.Rows) != 1 || r.Rows[0][0].S != payload {
			t.Errorf("payload %q was not stored verbatim: %v", payload, r.Rows)
		}
	}
	if got := rowsOf(run(t, s, "SELECT id FROM customers")); got != "1;2;42" {
		t.Fatalf("injection altered data: %s", got)
	}
	runErr(t, s, "SELECT id FROM customers WHERE id = ?", "1 parameter(s) but 0 value(s)")
	runErr(t, s, "SELECT id FROM customers", "0 parameter(s) but 1 value(s)", sql.Int(1))
}

func TestTransactionCommitAndRollback(t *testing.T) {
	ex, be := newExec()
	s := ex.NewSession()
	run(t, s, "CREATE TABLE accounts (id INT PRIMARY KEY, balance INT NOT NULL)")
	run(t, s, "INSERT INTO accounts VALUES (1, 100), (2, 0)")
	before := be.commits

	run(t, s, "BEGIN")
	run(t, s, "UPDATE accounts SET balance = balance - 30 WHERE id = 1")
	run(t, s, "UPDATE accounts SET balance = balance + 30 WHERE id = 2")
	// A transaction sees its own writes...
	if got := rowsOf(run(t, s, "SELECT balance FROM accounts WHERE id = 1")); got != "70" {
		t.Fatalf("transaction cannot read its own write: %s", got)
	}
	// ...which are not yet visible in the committed state.
	if r, _ := ex.DB.get("accounts", sql.EncodeKey([]sql.Value{sql.Int(1)})); r[1].I != 100 {
		t.Fatalf("uncommitted write leaked to the database")
	}
	if be.commits != before {
		t.Fatalf("nothing should replicate before COMMIT")
	}
	run(t, s, "COMMIT")
	// Atomicity comes from this: one transaction is one log entry.
	if be.commits != before+1 {
		t.Fatalf("a transaction should be exactly one log entry, got %d", be.commits-before)
	}
	if got := rowsOf(run(t, s, "SELECT balance FROM accounts")); got != "70;30" {
		t.Fatalf("got %s", got)
	}

	run(t, s, "BEGIN")
	run(t, s, "DELETE FROM accounts WHERE id = 1")
	run(t, s, "INSERT INTO accounts VALUES (3, 5)")
	if got := rowsOf(run(t, s, "SELECT id FROM accounts")); got != "2;3" {
		t.Fatalf("overlay scan: %s", got)
	}
	run(t, s, "ROLLBACK")
	if got := rowsOf(run(t, s, "SELECT id FROM accounts")); got != "1;2" {
		t.Fatalf("rollback left changes: %s", got)
	}
	if ex.Locks.Size() != 0 {
		t.Fatalf("locks leaked: %d", ex.Locks.Size())
	}
	runErr(t, s, "COMMIT", "no transaction")
	runErr(t, s, "ROLLBACK", "no transaction")
	run(t, s, "BEGIN")
	runErr(t, s, "BEGIN", "already open")
	runErr(t, s, "CREATE TABLE x (id INT PRIMARY KEY)", "not allowed inside a transaction")
	run(t, s, "ROLLBACK")
}

func TestFailedStatementAbortsTransaction(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	run(t, s, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	run(t, s, "INSERT INTO t VALUES (1, 1)")
	run(t, s, "BEGIN")
	run(t, s, "UPDATE t SET n = 99 WHERE id = 1")
	runErr(t, s, "INSERT INTO t VALUES (1, 2)", "transaction aborted")
	if s.InTransaction() {
		t.Fatalf("transaction should have ended")
	}
	if got := rowsOf(run(t, s, "SELECT n FROM t")); got != "1" {
		t.Fatalf("partial transaction took effect: %s", got)
	}
}

func TestReadOnlyCommitConfirmsLeadership(t *testing.T) {
	ex, be := newExec()
	s := ex.NewSession()
	run(t, s, "CREATE TABLE t (id INT PRIMARY KEY)")
	before := be.barriers.Load()
	run(t, s, "SELECT * FROM t")
	if be.barriers.Load() != before+1 {
		t.Fatalf("a read must confirm leadership before answering")
	}
}

func TestLeadershipLossAbortsOpenTransaction(t *testing.T) {
	ex, be := newExec()
	s := ex.NewSession()
	run(t, s, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	run(t, s, "INSERT INTO t VALUES (1, 1)")
	run(t, s, "BEGIN")
	run(t, s, "SELECT n FROM t WHERE id = 1")
	// Leadership moves away and back. The transaction's locks are gone.
	ex.Locks.SetEpoch(5)
	be.epoch = 5
	if _, err := s.Execute(context.Background(), "SELECT n FROM t WHERE id = 1", nil, ""); !errors.Is(err, txn.ErrStale) {
		t.Fatalf("a transaction whose locks vanished must abort, got %v", err)
	}
}

func TestRetriedRequestAppliesOnce(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	ctx := context.Background()
	run(t, s, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	if _, err := s.Execute(ctx, "INSERT INTO t VALUES (1, 0)", nil, "req-1"); err != nil {
		t.Fatal(err)
	}
	// The same request again must report success, not "duplicate primary key".
	r, err := s.Execute(ctx, "INSERT INTO t VALUES (1, 0)", nil, "req-1")
	if err != nil || !r.Duplicate || r.Affected != 1 {
		t.Fatalf("retry: %+v, %v", r, err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Execute(ctx, "UPDATE t SET n = n + 1 WHERE id = 1", nil, "req-2"); err != nil {
			t.Fatal(err)
		}
	}
	if got := rowsOf(run(t, s, "SELECT n FROM t")); got != "1" {
		t.Fatalf("increment applied more than once: %s", got)
	}
}

func TestSnapshotRoundTripIsExact(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	setupShop(t, s)
	if _, err := s.Execute(context.Background(), "UPDATE products SET price_cents = 1 WHERE id = 10", nil, "req-x"); err != nil {
		t.Fatal(err)
	}
	snap, err := ex.DB.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	other := NewDB()
	if err := other.Restore(snap); err != nil {
		t.Fatal(err)
	}
	again, err := other.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snap, again) {
		t.Fatalf("snapshot not canonical after a round trip")
	}
	if _, ok := other.seen("req-x"); !ok {
		t.Fatalf("request history lost in the snapshot, so a retry could apply twice")
	}
}

func TestStaleReadWorksWithoutLeadership(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	setupShop(t, s)
	ex.Backend = nil // a follower: any leader-only call would crash
	r, err := ex.ExecuteStale(context.Background(), "SELECT name FROM customers WHERE id = 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Stale || rowsOf(r) != "'Asha'" {
		t.Fatalf("got %+v", r)
	}
	if _, err := ex.ExecuteStale(context.Background(), "DELETE FROM customers", nil); err == nil {
		t.Fatalf("a stale read must not allow writes")
	}
}

func TestExplainShowsPipelineWithoutRunning(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	setupShop(t, s)
	r := run(t, s, "EXPLAIN DELETE FROM customers WHERE id = 1")
	for _, want := range []string{"Parse tree:", "delete_stmt", "Relational algebra:", "σ["} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("EXPLAIN lacks %q:\n%s", want, r.Message)
		}
	}
	if got := rowsOf(run(t, s, "SELECT id FROM customers WHERE id = 1")); got != "1" {
		t.Fatalf("EXPLAIN executed the statement")
	}
}

// ---------------------------------------------------------------------------
// Concurrency: the point of 2PL
// ---------------------------------------------------------------------------

func setupBank(t *testing.T, ex *Executor, accounts int, balance int64) {
	t.Helper()
	s := ex.NewSession()
	run(t, s, "CREATE TABLE accounts (id INT PRIMARY KEY, balance INT NOT NULL)")
	for i := 0; i < accounts; i++ {
		run(t, s, "INSERT INTO accounts VALUES (?, ?)", sql.Int(int64(i)), sql.Int(balance))
	}
}

func transfer(ctx context.Context, ex *Executor, from, to int, amount int64) error {
	s := ex.NewSession()
	defer s.Abort()
	steps := []struct {
		q string
		p []sql.Value
	}{
		{"BEGIN", nil},
		{"SELECT balance FROM accounts WHERE id = ?", []sql.Value{sql.Int(int64(from))}},
		{"UPDATE accounts SET balance = balance - ? WHERE id = ?", []sql.Value{sql.Int(amount), sql.Int(int64(from))}},
		{"UPDATE accounts SET balance = balance + ? WHERE id = ?", []sql.Value{sql.Int(amount), sql.Int(int64(to))}},
		{"COMMIT", nil},
	}
	for _, st := range steps {
		if _, err := s.Execute(ctx, st.q, st.p, ""); err != nil {
			return err
		}
	}
	return nil
}

// runBankWorkload drives concurrent transfers and audits. Audits read every account in
// one transaction, and must always see the same total, however transfers interleave.
func runBankWorkload(t *testing.T, ex *Executor, workers, rounds, accounts int, total int64) (torn int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var tornTotals atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < rounds; i++ {
				if r.Intn(5) == 0 {
					s := ex.NewSession()
					res, err := s.Execute(ctx, "SELECT balance FROM accounts", nil, "")
					if err == nil {
						var sum int64
						for _, row := range res.Rows {
							sum += row[0].I
						}
						if sum != total {
							tornTotals.Add(1)
						}
					}
					continue
				}
				from := r.Intn(accounts)
				to := (from + 1 + r.Intn(accounts-1)) % accounts
				// wait-die aborts some attempts. Retrying is the client's job.
				for attempt := 0; attempt < 200; attempt++ {
					err := transfer(ctx, ex, from, to, int64(r.Intn(50)+1))
					if err == nil || !errors.Is(err, txn.ErrDie) {
						if err != nil {
							t.Errorf("transfer: %v", err)
						}
						break
					}
					time.Sleep(time.Duration(r.Intn(2000)) * time.Microsecond)
				}
			}
		}(int64(w))
	}
	wg.Wait()
	return int(tornTotals.Load())
}

func finalTotal(t *testing.T, ex *Executor) int64 {
	t.Helper()
	s := ex.NewSession()
	res, err := s.Execute(context.Background(), "SELECT balance FROM accounts", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, row := range res.Rows {
		sum += row[0].I
	}
	return sum
}

func TestConcurrentTransfersAreSerializable(t *testing.T) {
	ex, _ := newExec()
	const accounts, each = 6, int64(1000)
	setupBank(t, ex, accounts, each)
	ex.Recorder = txn.NewRecorder()
	if torn := runBankWorkload(t, ex, 12, 60, accounts, accounts*each); torn != 0 {
		t.Errorf("%d audits saw money mid-transfer", torn)
	}
	if got := finalTotal(t, ex); got != accounts*each {
		t.Errorf("total is %d, want %d: money was created or destroyed", got, accounts*each)
	}
	sched := ex.Recorder.Schedule()
	res := txn.CheckConflictSerializable(sched)
	if !res.Serializable {
		t.Fatalf("the real schedule of %d actions is not conflict-serializable, cycle %v", len(sched), res.Cycle)
	}
	if len(res.Order) < 100 || len(res.Edges) < 100 {
		t.Fatalf("workload too small to mean anything: %d transactions, %d conflicts", len(res.Order), len(res.Edges))
	}
	t.Logf("%d actions, %d committed transactions, %d conflict edges: conflict-serializable",
		len(sched), len(res.Order), len(res.Edges))
	if ex.Locks.Size() != 0 {
		t.Errorf("locks leaked: %d", ex.Locks.Size())
	}
}

// The control experiment. If removing the locks did not break serializability, the test
// above would prove nothing. Every run is checked until a violation shows up.
func TestWithoutLocksTheScheduleIsNotSerializable(t *testing.T) {
	found := false
	for attempt := 0; attempt < 30 && !found; attempt++ {
		ex, _ := newExec()
		ex.DisableLocks = true
		setupBank(t, ex, 3, 1000)
		ex.Recorder = txn.NewRecorder()
		runBankWorkload(t, ex, 12, 40, 3, 3000)
		res := txn.CheckConflictSerializable(ex.Recorder.Schedule())
		found = !res.Serializable
		if found {
			t.Logf("without 2PL, attempt %d produced cycle %v", attempt, res.Cycle)
		}
	}
	if !found {
		t.Fatalf("removing locks never broke serializability, so the positive test is not evidence of anything")
	}
}

func TestPhantomsArePrevented(t *testing.T) {
	ex, _ := newExec()
	s1, s2 := ex.NewSession(), ex.NewSession()
	run(t, s1, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	run(t, s1, "INSERT INTO t VALUES (1, 1), (2, 2)")

	run(t, s1, "BEGIN")
	first := rowsOf(run(t, s1, "SELECT id FROM t WHERE n > 0"))

	// s2 is younger than s1, so wait-die makes its conflicting insert fail, not wait.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s2.autoRetries = 0
	if _, err := s2.Execute(ctx, "INSERT INTO t VALUES (3, 3)", nil, ""); err == nil {
		t.Fatalf("an insert into a scanned table must conflict until the scan's transaction ends")
	}
	if again := rowsOf(run(t, s1, "SELECT id FROM t WHERE n > 0")); again != first {
		t.Fatalf("phantom: first read %s, second read %s", first, again)
	}
	run(t, s1, "COMMIT")
	run(t, s2, "INSERT INTO t VALUES (3, 3)")
}

func TestRowLocksAllowDisjointWriters(t *testing.T) {
	ex, _ := newExec()
	setupBank(t, ex, 2, 100)
	s1, s2 := ex.NewSession(), ex.NewSession()
	run(t, s1, "BEGIN")
	run(t, s2, "BEGIN")
	run(t, s1, "UPDATE accounts SET balance = 1 WHERE id = 0")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s2.Execute(ctx, "UPDATE accounts SET balance = 2 WHERE id = 1", nil, ""); err != nil {
		t.Fatalf("writers on different rows should not block each other: %v", err)
	}
	run(t, s1, "COMMIT")
	run(t, s2, "COMMIT")
}

func TestManyDistinctTableRowsScanInKeyOrder(t *testing.T) {
	ex, _ := newExec()
	s := ex.NewSession()
	run(t, s, "CREATE TABLE t (a TEXT, b INT, PRIMARY KEY (a, b))")
	vals := []string{"b,2", "a,10", "a,9", "c,-1"}
	for _, v := range vals {
		parts := strings.Split(v, ",")
		var n int64
		fmt.Sscan(parts[1], &n)
		run(t, s, "INSERT INTO t VALUES (?, ?)", sql.Text(parts[0]), sql.Int(n))
	}
	if got := rowsOf(run(t, s, "SELECT * FROM t")); got != "'a',9;'a',10;'b',2;'c',-1" {
		t.Fatalf("got %s", got)
	}
}
