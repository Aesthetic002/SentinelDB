// Package engine is the replicated state machine: tables, the operations that change
// them, and the executor that turns SQL into those operations under two-phase locking.
package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"sentineldb/internal/sql"
)

// Row is one tuple. Rows are immutable once stored; updates install a new slice, so a
// reader holding a Row never sees it change.
type Row = []sql.Value

// Column is a column of a table.
type Column struct {
	Name    string   `json:"name"`
	Type    sql.Kind `json:"type"`
	NotNull bool     `json:"not_null,omitempty"`
}

// ForeignKey says Column must match the single-column primary key of RefTable.
type ForeignKey struct {
	Column   string `json:"column"`
	RefTable string `json:"ref_table"`
}

// Schema describes a table.
type Schema struct {
	Name    string       `json:"name"`
	Columns []Column     `json:"columns"`
	PK      []string     `json:"pk"`
	Foreign []ForeignKey `json:"foreign,omitempty"`
}

// ColumnIndex returns the position of a column, or -1.
func (s *Schema) ColumnIndex(name string) int {
	for i, c := range s.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// Key builds a row's encoded primary key.
func (s *Schema) Key(r Row) string {
	vals := make([]sql.Value, len(s.PK))
	for i, name := range s.PK {
		vals[i] = r[s.ColumnIndex(name)]
	}
	return sql.EncodeKey(vals)
}

type table struct {
	schema *Schema
	rows   map[string]Row
	// sorted caches the keys in order. It is rebuilt lazily after an insert or delete,
	// so scans are deterministic without paying to keep the order on every write.
	sorted []string
}

func (t *table) keys() []string {
	if t.sorted == nil {
		t.sorted = make([]string, 0, len(t.rows))
		for k := range t.rows {
			t.sorted = append(t.sorted, k)
		}
		sort.Strings(t.sorted)
	}
	return t.sorted
}

// OpKind is the kind of a physical change.
type OpKind uint8

const (
	OpCreateTable OpKind = iota + 1
	OpPut
	OpDelete
)

// Op is one physical change. The log holds these rather than SQL text: SQL was already
// evaluated once by the leader under locks, so followers need no re-execution, and
// replay cannot diverge between replicas.
type Op struct {
	Kind   OpKind  `json:"k"`
	Schema *Schema `json:"s,omitempty"`
	Table  string  `json:"t,omitempty"`
	Key    string  `json:"key,omitempty"`
	Row    Row     `json:"r,omitempty"`
}

// Command is the unit of replication: one transaction's whole write set in one log
// entry. A single entry either commits or does not, which is what makes a multi-row
// transaction atomic across a crash.
type Command struct {
	// RequestID makes a client's retry idempotent; empty disables deduplication.
	RequestID string `json:"id,omitempty"`
	Ops       []Op   `json:"ops"`
	// Affected is the statement's row count, returned again to a retried request.
	Affected int `json:"n,omitempty"`
}

// ApplyResult is what applying a command produced.
type ApplyResult struct {
	Affected int
	// Duplicate means the request ID was seen before and the command changed nothing.
	Duplicate bool
	Err       string
}

const dedupeCapacity = 8192

// DB is the in-memory state machine. Durability comes from the Raft log and snapshots
// beneath it, and Apply must stay deterministic: every replica runs it on the same
// commands in the same order and must reach the same state.
type DB struct {
	mu     sync.RWMutex
	tables map[string]*table

	dedupe      map[string]int
	dedupeOrder []string
}

// NewDB returns an empty database.
func NewDB() *DB {
	return &DB{tables: map[string]*table{}, dedupe: map[string]int{}}
}

// Apply executes a command atomically.
func (db *DB) Apply(c *Command) ApplyResult {
	db.mu.Lock()
	defer db.mu.Unlock()
	if c.RequestID != "" {
		if n, ok := db.dedupe[c.RequestID]; ok {
			return ApplyResult{Affected: n, Duplicate: true}
		}
	}
	res := ApplyResult{Affected: c.Affected}
	for i := range c.Ops {
		op := &c.Ops[i]
		switch op.Kind {
		case OpCreateTable:
			if _, exists := db.tables[op.Schema.Name]; exists {
				// Reachable only if two creates race past their locks across a leader
				// change. Every replica reaches this same branch, so state stays equal.
				res.Err = fmt.Sprintf("table %s already exists", op.Schema.Name)
				continue
			}
			db.tables[op.Schema.Name] = &table{schema: op.Schema, rows: map[string]Row{}}
		case OpPut:
			t := db.tables[op.Table]
			if t == nil {
				res.Err = fmt.Sprintf("table %s does not exist", op.Table)
				continue
			}
			if _, exists := t.rows[op.Key]; !exists {
				t.sorted = nil
			}
			t.rows[op.Key] = op.Row
		case OpDelete:
			if t := db.tables[op.Table]; t != nil {
				if _, exists := t.rows[op.Key]; exists {
					delete(t.rows, op.Key)
					t.sorted = nil
				}
			}
		}
	}
	if c.RequestID != "" {
		db.dedupe[c.RequestID] = c.Affected
		db.dedupeOrder = append(db.dedupeOrder, c.RequestID)
		if len(db.dedupeOrder) > dedupeCapacity {
			delete(db.dedupe, db.dedupeOrder[0])
			db.dedupeOrder = db.dedupeOrder[1:]
		}
	}
	return res
}

// seen reports the recorded row count if a request ID has already been applied.
func (db *DB) seen(id string) (int, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	n, ok := db.dedupe[id]
	return n, ok
}

// Schema returns a table's schema, or nil.
func (db *DB) Schema(name string) *Schema {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if t := db.tables[name]; t != nil {
		return t.schema
	}
	return nil
}

// TableNames returns all tables, sorted.
func (db *DB) TableNames() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]string, 0, len(db.tables))
	for n := range db.tables {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// get returns a committed row.
func (db *DB) get(tableName, key string) (Row, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t := db.tables[tableName]
	if t == nil {
		return nil, false
	}
	r, ok := t.rows[key]
	return r, ok
}

type keyedRow struct {
	key string
	row Row
}

// scan returns a table's committed rows in key order.
func (db *DB) scan(tableName string) []keyedRow {
	// A write lock, because keys() may rebuild the cache. Scans run under table-level
	// S locks at statement scope, so this is brief relative to the work around it.
	db.mu.Lock()
	defer db.mu.Unlock()
	t := db.tables[tableName]
	if t == nil {
		return nil
	}
	keys := t.keys()
	out := make([]keyedRow, len(keys))
	for i, k := range keys {
		out[i] = keyedRow{k, t.rows[k]}
	}
	return out
}

// childrenOf lists tables with a foreign key referencing the given table.
func (db *DB) childrenOf(tableName string) []*Schema {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []*Schema
	for _, t := range db.tables {
		for _, fk := range t.schema.Foreign {
			if fk.RefTable == tableName {
				out = append(out, t.schema)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type snapshotTable struct {
	Schema *Schema `json:"schema"`
	Keys   []string
	Rows   []Row
}

type snapshotState struct {
	Tables      []snapshotTable `json:"tables"`
	DedupeOrder []string        `json:"dedupe_order"`
	DedupeVals  []int           `json:"dedupe_vals"`
}

// Snapshot serializes the state machine. Output is canonical (sorted), so two replicas
// in the same state produce identical bytes, which tests rely on to compare replicas.
func (db *DB) Snapshot() ([]byte, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	st := snapshotState{DedupeOrder: db.dedupeOrder}
	names := make([]string, 0, len(db.tables))
	for n := range db.tables {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := db.tables[n]
		keys := t.keys()
		rows := make([]Row, len(keys))
		for i, k := range keys {
			rows[i] = t.rows[k]
		}
		st.Tables = append(st.Tables, snapshotTable{t.schema, keys, rows})
	}
	st.DedupeVals = make([]int, len(db.dedupeOrder))
	for i, id := range db.dedupeOrder {
		st.DedupeVals[i] = db.dedupe[id]
	}
	return json.Marshal(st)
}

// Restore replaces the state machine with a snapshot. Nil or empty data resets it.
func (db *DB) Restore(data []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.tables = map[string]*table{}
	db.dedupe = map[string]int{}
	db.dedupeOrder = nil
	if len(data) == 0 {
		return nil
	}
	var st snapshotState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("restoring snapshot: %w", err)
	}
	for _, s := range st.Tables {
		t := &table{schema: s.Schema, rows: make(map[string]Row, len(s.Keys))}
		for i, k := range s.Keys {
			t.rows[k] = s.Rows[i]
		}
		db.tables[s.Schema.Name] = t
	}
	db.dedupeOrder = st.DedupeOrder
	for i, id := range st.DedupeOrder {
		db.dedupe[id] = st.DedupeVals[i]
	}
	return nil
}
