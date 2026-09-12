package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNewGateRejectsNonPositiveCapacity(t *testing.T) {
	if _, err := NewGate(0); err == nil {
		t.Error("NewGate(0) should fail")
	}
	if _, err := NewGate(-1); err == nil {
		t.Error("NewGate(-1) should fail")
	}
}

func TestGateCapsConcurrency(t *testing.T) {
	g, err := NewGate(2)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	ctx := context.Background()

	for range 2 {
		if err := g.Acquire(ctx); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}
	if stats := g.Stats(); stats.Held != 2 {
		t.Fatalf("held = %d, want 2", stats.Held)
	}

	// A third caller must wait rather than oversubscribing the UI VM.
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := g.Acquire(blocked); err == nil {
		t.Fatal("the third acquire should have waited and timed out")
	}

	g.Release()
	if err := g.Acquire(ctx); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
}

func TestGateIsFirstInFirstOut(t *testing.T) {
	g, err := NewGate(1)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	ctx := context.Background()
	if err := g.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	const waiters = 8
	var (
		mu     sync.Mutex
		order  []int
		wg     sync.WaitGroup
		queued sync.WaitGroup
	)
	queued.Add(waiters)
	for i := range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Signal that this goroutine is about to queue, then queue.
			queued.Done()
			if err := g.Acquire(ctx); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			g.Release()
		}()
		// Serialise enqueueing so the expected order is well defined.
		waitForWaiting(t, g, i+1)
	}
	queued.Wait()

	g.Release()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != waiters {
		t.Fatalf("got %d completions, want %d", len(order), waiters)
	}
	for i, got := range order {
		if got != i {
			t.Fatalf("slots were handed out in order %v, want ascending: a later caller overtook an earlier one", order)
		}
	}
}

// waitForWaiting blocks until the gate reports n queued waiters.
func waitForWaiting(t *testing.T, g *Gate, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if g.Stats().Waiting >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d queued waiters (stats: %+v)", n, g.Stats())
}

func TestGateCancelledWaiterDoesNotStealASlot(t *testing.T) {
	g, err := NewGate(1)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	ctx := context.Background()
	if err := g.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// One caller queues and then gives up.
	giveUp, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- g.Acquire(giveUp) }()
	waitForWaiting(t, g, 1)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the cancelled caller should have reported an error")
	}

	// The slot must reach the next live caller, not be lost with the
	// abandoned one.
	got := make(chan error, 1)
	go func() { got <- g.Acquire(ctx) }()
	waitForWaiting(t, g, 1)
	g.Release()

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the slot was lost to the abandoned waiter")
	}
}

func TestGateDoReleasesOnPanic(t *testing.T) {
	g, err := NewGate(1)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}

	func() {
		defer func() { _ = recover() }()
		//nolint:errcheck // the panic is the point
		g.Do(context.Background(), func(context.Context) error { panic("boom") })
	}()

	if stats := g.Stats(); stats.Held != 0 {
		t.Fatalf("held = %d after a panicking job, want 0: the slot leaked", stats.Held)
	}
}

func TestGateDoRunsAndReleases(t *testing.T) {
	g, err := NewGate(1)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	ran := false
	if err := g.Do(context.Background(), func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !ran {
		t.Fatal("Do did not run the function")
	}
	if stats := g.Stats(); stats.Held != 0 {
		t.Fatalf("held = %d after Do, want 0", stats.Held)
	}
}

func TestGateUnderContention(t *testing.T) {
	const capacity = 4
	g, err := NewGate(capacity)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}

	var (
		mu      sync.Mutex
		inside  int
		highest int
		wg      sync.WaitGroup
	)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			//nolint:errcheck // errors are asserted via the counters
			g.Do(context.Background(), func(context.Context) error {
				mu.Lock()
				inside++
				highest = max(highest, inside)
				mu.Unlock()

				time.Sleep(time.Millisecond)

				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if highest > capacity {
		t.Fatalf("peak concurrency was %d, above the cap of %d", highest, capacity)
	}
	if stats := g.Stats(); stats.Held != 0 || stats.Waiting != 0 {
		t.Fatalf("gate did not drain: %+v", stats)
	}
}
