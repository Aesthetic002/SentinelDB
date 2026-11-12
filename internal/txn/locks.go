// Package txn implements strict two-phase locking and a conflict-serializability
// checker for the schedules it produces.
package txn

import (
	"context"
	"errors"
	"sync"
)

// Mode is a lock mode. Intention modes (IS, IX) let row locks and table locks coexist:
// a table scan takes S on the table, which conflicts with the IX every writer must
// hold first, so a scan cannot miss a row inserted mid-scan (no phantoms).
type Mode uint8

const (
	IS Mode = iota + 1
	IX
	S
	X
)

func (m Mode) String() string { return [...]string{"-", "IS", "IX", "S", "X"}[m] }

// compatible is the standard multi-granularity locking matrix.
func compatible(a, b Mode) bool {
	switch a {
	case IS:
		return b != X
	case IX:
		return b == IS || b == IX
	case S:
		return b == IS || b == S
	}
	return false
}

// covers reports whether holding a already grants everything b would.
func covers(a, b Mode) bool {
	switch a {
	case X:
		return true
	case S:
		return b == S || b == IS
	case IX:
		return b == IX || b == IS
	case IS:
		return b == IS
	}
	return false
}

// combine returns the weakest mode at least as strong as both. There is no SIX mode
// here, so S+IX conservatively becomes X.
func combine(a, b Mode) Mode {
	switch {
	case a == 0 || covers(b, a):
		return b
	case covers(a, b):
		return a
	}
	return X
}

var (
	// ErrDie means the wait-die rule aborted this transaction to prevent deadlock.
	ErrDie = errors.New("transaction aborted to avoid deadlock; retry it")
	// ErrStale means leadership changed, so this transaction's locks no longer exist.
	ErrStale = errors.New("transaction aborted: leadership changed")
)

type waiter struct {
	tx   uint64
	mode Mode
	ch   chan struct{}
}

type lockState struct {
	holders map[uint64]Mode
	queue   []*waiter
}

// Manager is the lock table. It lives only on the current leader: locks are not
// replicated, so when leadership changes they vanish, and SetEpoch aborts every
// transaction that was relying on them.
type Manager struct {
	mu     sync.Mutex
	locks  map[string]*lockState
	byTx   map[uint64]map[string]bool
	epoch  uint64
	nextTx uint64
}

// NewManager returns an empty lock table.
func NewManager() *Manager {
	return &Manager{locks: map[string]*lockState{}, byTx: map[uint64]map[string]bool{}}
}

// NewTxID allocates a transaction ID. IDs grow over time, so a lower ID is an older
// transaction, which is the ordering wait-die needs. A retried transaction should keep
// its ID: it stays old and eventually wins instead of starving.
func (m *Manager) NewTxID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextTx++
	return m.nextTx
}

// SetEpoch drops all locks and fails all waiters. The server calls it on every change
// of leadership, so a transaction from an earlier term cannot continue believing it
// still holds locks.
func (m *Manager) SetEpoch(epoch uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epoch = epoch
	for _, l := range m.locks {
		for _, w := range l.queue {
			close(w.ch)
		}
	}
	m.locks = map[string]*lockState{}
	m.byTx = map[uint64]map[string]bool{}
}

// Epoch returns the current epoch.
func (m *Manager) Epoch() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.epoch
}

// Acquire blocks until tx holds res in at least the given mode.
//
// Deadlock is impossible by the wait-die rule: a transaction may only wait for younger
// ones and dies rather than wait for an older one, so waits-for edges always point from
// older to younger and can never form a cycle.
func (m *Manager) Acquire(ctx context.Context, epoch, tx uint64, res string, mode Mode) error {
	m.mu.Lock()
	for {
		if epoch != m.epoch {
			m.mu.Unlock()
			return ErrStale
		}
		l := m.locks[res]
		if l == nil {
			l = &lockState{holders: map[uint64]Mode{}}
			m.locks[res] = l
		}
		held := l.holders[tx]
		if held != 0 && covers(held, mode) {
			m.mu.Unlock()
			return nil
		}
		want := combine(held, mode)

		conflict, older := false, false
		for other, om := range l.holders {
			if other != tx && !compatible(want, om) {
				conflict = true
				older = older || other < tx
			}
		}
		// Respect earlier waiters so a stream of readers cannot starve a writer. An
		// upgrade skips this: it already holds a lock, and queueing behind a waiter who
		// is blocked on that same lock would be a deadlock.
		if held == 0 {
			for _, w := range l.queue {
				if w.tx != tx && !compatible(want, w.mode) {
					conflict = true
					older = older || w.tx < tx
				}
			}
		}
		if !conflict {
			l.holders[tx] = want
			if m.byTx[tx] == nil {
				m.byTx[tx] = map[string]bool{}
			}
			m.byTx[tx][res] = true
			m.mu.Unlock()
			return nil
		}
		if older {
			m.cleanup(res, l)
			m.mu.Unlock()
			return ErrDie
		}

		w := &waiter{tx: tx, mode: want, ch: make(chan struct{})}
		l.queue = append(l.queue, w)
		m.mu.Unlock()

		select {
		case <-w.ch:
			m.mu.Lock()
		case <-ctx.Done():
			m.mu.Lock()
			m.removeWaiter(res, w)
			m.mu.Unlock()
			return ctx.Err()
		}
	}
}

func (m *Manager) removeWaiter(res string, w *waiter) {
	l := m.locks[res]
	if l == nil {
		return
	}
	for i, q := range l.queue {
		if q == w {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			// Waiters queued behind this one may now be able to proceed.
			wake(l)
			break
		}
	}
	m.cleanup(res, l)
}

func wake(l *lockState) {
	for _, w := range l.queue {
		close(w.ch)
	}
	l.queue = nil
}

func (m *Manager) cleanup(res string, l *lockState) {
	if len(l.holders) == 0 && len(l.queue) == 0 {
		delete(m.locks, res)
	}
}

// ReleaseAll drops every lock tx holds. This is the only release point, and it comes
// after commit, which makes this strict 2PL: no other transaction can see or overwrite
// a change until it is durable, and there is no growing phase after any lock is
// released.
func (m *Manager) ReleaseAll(tx uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for res := range m.byTx[tx] {
		l := m.locks[res]
		if l == nil {
			continue
		}
		delete(l.holders, tx)
		wake(l)
		m.cleanup(res, l)
	}
	delete(m.byTx, tx)
}

// Held returns how many resources tx has locked.
func (m *Manager) Held(tx uint64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byTx[tx])
}

// Size returns how many resources are currently tracked, for leak tests.
func (m *Manager) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}
