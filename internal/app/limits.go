package app

import (
	"context"
	"sync"
)

// concurrencyLimit survives reloads so old and new requests share the same budget.
type concurrencyLimit struct {
	mu          sync.Mutex
	limit, used int
	changed     chan struct{}
}

func newConcurrencyLimit(n int) *concurrencyLimit {
	return &concurrencyLimit{limit: n, changed: make(chan struct{})}
}
func (l *concurrencyLimit) acquire(ctx context.Context) bool {
	for {
		l.mu.Lock()
		if l.used < l.limit {
			l.used++
			l.mu.Unlock()
			return true
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}
func (l *concurrencyLimit) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used--
	close(l.changed)
	l.changed = make(chan struct{})
}
func (l *concurrencyLimit) resize(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = n
	close(l.changed)
	l.changed = make(chan struct{})
}
