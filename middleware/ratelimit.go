package middleware

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aigateway/models"
)

// ConcurrencyLimiter uses buffered channels as semaphores to limit
// the number of concurrent requests at global, per-provider, and per-model levels.
// Supports bounded request queuing when all slots are full.
type ConcurrencyLimiter struct {
	global       chan struct{}
	queueDepth   int
	queueTimeout time.Duration
	queued       atomic.Int64 // current number of waiters (bounded by queueDepth)

	perProvider int
	perModel    int

	mu            sync.Mutex
	providerSlots map[string]chan struct{}
	modelSlots    map[string]chan struct{}
}

// NewConcurrencyLimiter creates a new limiter with the given global max.
func NewConcurrencyLimiter(globalMax int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		global:        make(chan struct{}, globalMax),
		queueDepth:    50,
		queueTimeout:  30 * time.Second,
		providerSlots: make(map[string]chan struct{}),
		modelSlots:    make(map[string]chan struct{}),
	}
}

// NewConcurrencyLimiterWithQueue creates a new limiter with queue support plus
// per-provider and per-model concurrency caps (<=0 means unlimited).
func NewConcurrencyLimiterWithQueue(globalMax, queueDepth, perProvider, perModel int, queueTimeout time.Duration) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		global:        make(chan struct{}, globalMax),
		queueDepth:    queueDepth,
		queueTimeout:  queueTimeout,
		perProvider:   perProvider,
		perModel:      perModel,
		providerSlots: make(map[string]chan struct{}),
		modelSlots:    make(map[string]chan struct{}),
	}
}

// AcquireGlobal tries to acquire a global concurrency slot.
// Returns true if acquired, false if at capacity.
func (cl *ConcurrencyLimiter) AcquireGlobal() bool {
	select {
	case cl.global <- struct{}{}:
		return true
	default:
		return false
	}
}

// AcquireGlobalWithQueue tries to acquire a global concurrency slot.
// If no slot is available, queues up to queueDepth waiters, each waiting at
// most queueTimeout for a slot to free up. Returns false immediately when the
// queue is full ("queue full") or when the wait times out.
func (cl *ConcurrencyLimiter) AcquireGlobalWithQueue() bool {
	// Try immediate acquire first
	select {
	case cl.global <- struct{}{}:
		return true
	default:
	}

	// Bounded queue: refuse when too many requests are already waiting.
	if cl.queueDepth > 0 && cl.queued.Add(1) > int64(cl.queueDepth) {
		cl.queued.Add(-1)
		return false
	}
	if cl.queueDepth <= 0 {
		// Queueing disabled — fail fast instead of waiting unbounded.
		return false
	}
	defer cl.queued.Add(-1)

	// Wait for a slot with timeout
	timeout := cl.queueTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case cl.global <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// QueuedCount returns the number of requests currently waiting for a slot.
func (cl *ConcurrencyLimiter) QueuedCount() int {
	return int(cl.queued.Load())
}

// acquireNamed takes a slot from a lazily-created per-name semaphore.
// A non-positive limit (or empty name) means unlimited — always succeeds.
func (cl *ConcurrencyLimiter) acquireNamed(slots map[string]chan struct{}, limit int, name string) bool {
	if limit <= 0 || name == "" {
		return true
	}
	cl.mu.Lock()
	ch, ok := slots[name]
	if !ok {
		ch = make(chan struct{}, limit)
		slots[name] = ch
	}
	cl.mu.Unlock()

	select {
	case ch <- struct{}{}:
		return true
	default:
	}

	timeout := cl.queueTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ch <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// releaseNamed frees a previously acquired per-name slot.
func (cl *ConcurrencyLimiter) releaseNamed(slots map[string]chan struct{}, name string) {
	if name == "" {
		return
	}
	cl.mu.Lock()
	ch, ok := slots[name]
	cl.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// AcquireProvider takes a per-provider concurrency slot. Fails when the
// provider is saturated (caller should fail over or return 429).
func (cl *ConcurrencyLimiter) AcquireProvider(name string) bool {
	return cl.acquireNamed(cl.providerSlots, cl.perProvider, name)
}

// ReleaseProvider frees a per-provider concurrency slot.
func (cl *ConcurrencyLimiter) ReleaseProvider(name string) {
	cl.releaseNamed(cl.providerSlots, name)
}

// AcquireModel takes a per-model concurrency slot. Fails when saturated.
func (cl *ConcurrencyLimiter) AcquireModel(name string) bool {
	return cl.acquireNamed(cl.modelSlots, cl.perModel, name)
}

// ReleaseModel frees a per-model concurrency slot.
func (cl *ConcurrencyLimiter) ReleaseModel(name string) {
	cl.releaseNamed(cl.modelSlots, name)
}

// ReleaseGlobal releases a global concurrency slot.
func (cl *ConcurrencyLimiter) ReleaseGlobal() {
	<-cl.global
}

// ActiveCount returns the number of currently active requests.
func (cl *ConcurrencyLimiter) ActiveCount() int {
	return len(cl.global)
}

// Capacity returns the maximum concurrent requests.
func (cl *ConcurrencyLimiter) Capacity() int {
	return cap(cl.global)
}

// TooManyRequestsResponse sends a 429 error in OpenAI format.
func TooManyRequestsResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: "Too many concurrent requests. Please retry later.",
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
		},
	})
}

// QueueFullResponse sends a 429 error indicating the queue is full.
func QueueFullResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: "Server is at capacity and request queue is full. Please retry later.",
			Type:    "rate_limit_error",
			Code:    "queue_full",
		},
	})
}
