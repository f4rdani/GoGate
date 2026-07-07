package balancer

import (
	"sync"
	"testing"
)

func TestNew(t *testing.T) {
	rr := New([]int{0, 1, 2})
	if rr == nil {
		t.Fatal("New returned nil")
	}
	if rr.Len() != 3 {
		t.Errorf("expected length 3, got %d", rr.Len())
	}
}

func TestNextRoundRobin(t *testing.T) {
	rr := New([]string{"a", "b", "c"})

	results := make([]string, 6)
	for i := 0; i < 6; i++ {
		results[i] = rr.Next()
	}

	// Should cycle: a, b, c, a, b, c
	expected := []string{"a", "b", "c", "a", "b", "c"}
	for i, got := range results {
		if got != expected[i] {
			t.Errorf("iteration %d: expected '%s', got '%s'", i, expected[i], got)
		}
	}
}

func TestNextSingleItem(t *testing.T) {
	rr := New([]int{42})
	for i := 0; i < 5; i++ {
		if got := rr.Next(); got != 42 {
			t.Errorf("expected 42, got %d", got)
		}
	}
}

func TestLen(t *testing.T) {
	tests := []struct {
		items    []int
		expected int
	}{
		{[]int{}, 0},
		{[]int{1}, 1},
		{[]int{1, 2, 3}, 3},
	}
	for _, tt := range tests {
		rr := New(tt.items)
		if rr.Len() != tt.expected {
			t.Errorf("Len() = %d, want %d", rr.Len(), tt.expected)
		}
	}
}

func TestAll(t *testing.T) {
	items := []int{10, 20, 30}
	rr := New(items)
	all := rr.All()
	if len(all) != 3 {
		t.Errorf("expected 3 items, got %d", len(all))
	}
	for i, v := range all {
		if v != items[i] {
			t.Errorf("All()[%d] = %d, want %d", i, v, items[i])
		}
	}
}

func TestConcurrentNext(t *testing.T) {
	items := []int{0, 1, 2, 3, 4}
	rr := New(items)
	counts := make([]int, len(items))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := rr.Next()
			mu.Lock()
			counts[v]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Each item should be selected exactly 200 times (1000/5)
	for i, count := range counts {
		if count != 200 {
			t.Errorf("item %d selected %d times, expected 200", i, count)
		}
	}
}

func TestDistribution(t *testing.T) {
	rr := New([]string{"x", "y"})
	counts := map[string]int{"x": 0, "y": 0}

	for i := 0; i < 100; i++ {
		counts[rr.Next()]++
	}

	if counts["x"] != 50 || counts["y"] != 50 {
		t.Errorf("expected 50/50 distribution, got x=%d y=%d", counts["x"], counts["y"])
	}
}
