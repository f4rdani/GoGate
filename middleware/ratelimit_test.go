package middleware

import (
	"testing"
	"time"
)

func TestQueueDepthZeroFailsFast(t *testing.T) {
	l := NewConcurrencyLimiterWithQueue(1, 0, 0, 0, time.Second)
	if !l.AcquireGlobalWithQueue() {
		t.Fatal("first acquire should succeed")
	}
	defer l.ReleaseGlobal()
	if l.AcquireGlobalWithQueue() {
		t.Fatal("queueing disabled (depth 0) must fail fast when full")
	}
}

func TestQueueFullReturnsFalse(t *testing.T) {
	l := NewConcurrencyLimiterWithQueue(1, 1, 0, 0, 5*time.Second)
	if !l.AcquireGlobalWithQueue() {
		t.Fatal("first acquire should succeed")
	}
	defer l.ReleaseGlobal()

	// Occupy the single queue slot with a waiter.
	acquired := make(chan bool, 1)
	go func() { acquired <- l.AcquireGlobalWithQueue() }()
	// Wait until the waiter is actually queued.
	deadline := time.Now().Add(2 * time.Second)
	for l.QueuedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if l.QueuedCount() == 0 {
		t.Fatal("waiter never queued")
	}
	// Queue is full now — must refuse immediately.
	start := time.Now()
	if l.AcquireGlobalWithQueue() {
		t.Fatal("expected refusal when queue is full")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("queue-full refusal must be immediate, not a timeout wait")
	}
	l.ReleaseGlobal() // let the waiter proceed
	if !<-acquired {
		t.Fatal("queued waiter should acquire after release")
	}
}

func TestPerProviderLimit(t *testing.T) {
	l := NewConcurrencyLimiterWithQueue(10, 10, 1, 0, 50*time.Millisecond)
	if !l.AcquireProvider("p1") {
		t.Fatal("first provider acquire should succeed")
	}
	defer l.ReleaseProvider("p1")
	if l.AcquireProvider("p1") {
		t.Fatal("second acquire on saturated provider must fail")
	}
	if !l.AcquireProvider("p2") {
		t.Fatal("different provider must have its own budget")
	}
	l.ReleaseProvider("p2")
	l.ReleaseProvider("p1")
	if !l.AcquireProvider("p1") {
		t.Fatal("acquire should succeed after release")
	}
	l.ReleaseProvider("p1")
}

func TestPerModelUnlimitedWhenZero(t *testing.T) {
	l := NewConcurrencyLimiterWithQueue(10, 10, 0, 0, time.Second)
	for i := 0; i < 5; i++ {
		if !l.AcquireModel("m") {
			t.Fatal("zero per-model limit must mean unlimited")
		}
	}
	for i := 0; i < 5; i++ {
		l.ReleaseModel("m")
	}
}
