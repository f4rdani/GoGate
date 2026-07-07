package balancer

import "sync/atomic"

// RoundRobin implements a thread-safe, lock-free round-robin selector.
// Uses atomic counter for zero-contention concurrent access.
type RoundRobin[T any] struct {
	items   []T
	counter atomic.Uint64
}

// New creates a new RoundRobin balancer with the given items.
func New[T any](items []T) *RoundRobin[T] {
	return &RoundRobin[T]{items: items}
}

// Next returns the next item in round-robin order.
// Thread-safe via atomic increment.
func (rr *RoundRobin[T]) Next() T {
	n := rr.counter.Add(1) - 1
	return rr.items[n%uint64(len(rr.items))]
}

// Len returns the number of items.
func (rr *RoundRobin[T]) Len() int {
	return len(rr.items)
}

// All returns all items.
func (rr *RoundRobin[T]) All() []T {
	return rr.items
}
