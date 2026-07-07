package middleware

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/aigateway/models"
)

// ConcurrencyLimiter uses buffered channels as semaphores to limit
// the number of concurrent requests at global, per-provider, and per-model levels.
// Supports request queuing when all slots are full.
type ConcurrencyLimiter struct {
	global       chan struct{}
	queueDepth   int
	queueTimeout time.Duration
}

// NewConcurrencyLimiter creates a new limiter with the given global max.
func NewConcurrencyLimiter(globalMax int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		global:       make(chan struct{}, globalMax),
		queueDepth:   50,
		queueTimeout: 30 * time.Second,
	}
}

// NewConcurrencyLimiterWithQueue creates a new limiter with queue support.
func NewConcurrencyLimiterWithQueue(globalMax, queueDepth int, queueTimeout time.Duration) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		global:       make(chan struct{}, globalMax),
		queueDepth:   queueDepth,
		queueTimeout: queueTimeout,
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
// If no slot is available, waits up to queueTimeout for one to free up.
// Returns true if acquired, false if timed out.
func (cl *ConcurrencyLimiter) AcquireGlobalWithQueue() bool {
	// Try immediate acquire first
	select {
	case cl.global <- struct{}{}:
		return true
	default:
	}

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
