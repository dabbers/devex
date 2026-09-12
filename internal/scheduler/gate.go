package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Gate is a counting semaphore that hands out slots strictly in the order they
// were requested.
//
// It backs the verification queue: the shared UI VM serves every verifier from
// one browser with N independently addressable profiles, so concurrency is
// capped by that VM's capacity. When every profile is busy, new verification
// jobs queue and wait -- the same policy the fork scheduler applies to machine
// capacity, rather than a second, different behaviour.
//
// A plain buffered channel would not do: Go wakes a random blocked receiver,
// so a job could wait behind later arrivals indefinitely.
type Gate struct {
	mu       sync.Mutex
	capacity int
	held     int
	// waiters is a FIFO queue of goroutines blocked in Acquire. Each is woken
	// by closing its own channel, so the oldest waiter always wins.
	waiters []*waiter
}

type waiter struct {
	ready chan struct{}
	// cancelled marks a waiter whose caller gave up, so Release skips it.
	cancelled bool
}

// NewGate returns a gate admitting capacity holders at once.
func NewGate(capacity int) (*Gate, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("scheduler: gate capacity must be positive, got %d", capacity)
	}
	return &Gate{capacity: capacity}, nil
}

// Acquire claims a slot, waiting in line until one frees up or ctx is done.
func (g *Gate) Acquire(ctx context.Context) error {
	g.mu.Lock()
	// Jumping the queue when slots are free is only safe if nobody is already
	// waiting; otherwise a new arrival could overtake a queued caller.
	if g.held < g.capacity && len(g.waiters) == 0 {
		g.held++
		g.mu.Unlock()
		return nil
	}

	w := &waiter{ready: make(chan struct{})}
	g.waiters = append(g.waiters, w)
	g.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		select {
		case <-w.ready:
			// The slot was handed over as the context expired. Take ownership
			// and immediately release it, so it is never lost.
			g.mu.Unlock()
			g.Release()
		default:
			w.cancelled = true
			g.mu.Unlock()
		}
		return fmt.Errorf("scheduler: gave up waiting for a verification slot: %w", ctx.Err())
	}
}

// Release returns a slot, handing it to the longest-waiting caller if there is
// one.
func (g *Gate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()

	for len(g.waiters) > 0 {
		w := g.waiters[0]
		g.waiters = g.waiters[1:]
		if w.cancelled {
			// This caller has gone; the slot passes to the next in line.
			continue
		}
		close(w.ready)
		return
	}

	if g.held > 0 {
		g.held--
	}
}

// Do acquires a slot, runs fn, and releases the slot even if fn panics.
func (g *Gate) Do(ctx context.Context, fn func(context.Context) error) error {
	if err := g.Acquire(ctx); err != nil {
		return err
	}
	defer g.Release()
	return fn(ctx)
}

// Stats reports gate occupancy for the web UI.
type Stats struct {
	Capacity int `json:"capacity"`
	Held     int `json:"held"`
	Waiting  int `json:"waiting"`
}

// Stats returns a snapshot of occupancy.
func (g *Gate) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()

	waiting := 0
	for _, w := range g.waiters {
		if !w.cancelled {
			waiting++
		}
	}
	return Stats{Capacity: g.capacity, Held: g.held, Waiting: waiting}
}

// ErrGateClosed is reserved for future use when a gate is shut down.
var ErrGateClosed = errors.New("scheduler: gate closed")
