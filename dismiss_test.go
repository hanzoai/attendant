package main

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// left runs work and reports how many goroutines it left behind.
//
// The baseline is taken INSIDE the call, because a subtest runs on a goroutine of
// its own: a baseline taken outside reports one leak for every subtest and none of
// them is real. It also waits — a goroutine that has been signalled is not gone
// yet, so counting straight after the signal measures the scheduler.
//
// TestTheDetectorSeesALeak is what says this measurement works at all.
func left(work func()) int {
	runtime.GC()
	base := runtime.NumGoroutine()
	work()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.Gosched()
		over := runtime.NumGoroutine() - base
		if over <= 0 || time.Now().After(deadline) {
			return over
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTheDetectorSeesALeak is the known-positive. `left` reporting zero is only
// evidence if it can report non-zero, and the shape it must catch is the one this
// package shipped: a watch that wakes on ctx.Done() and nothing else, dismissed by
// a call on a context nobody cancels.
func TestTheDetectorSeesALeak(t *testing.T) {
	const n = 50
	var parked []context.CancelFunc
	over := left(func() {
		for range n {
			ctx, cancel := context.WithCancel(context.Background())
			parked = append(parked, cancel)
			go func() { <-ctx.Done() }() // the old shape: only a context retires it
		}
	})
	if over < n {
		t.Fatalf("the detector saw %d of %d parked goroutines — it cannot see a leak, so a zero from it means nothing", over, n)
	}
	for _, cancel := range parked {
		cancel()
	}
}

// TestDismissRetiresItsWatch is the leak. An attendant is dismissed by cancelling
// its context OR by calling Leave, and the second is what Attend's doc blesses —
// so a watch that only ever wakes on ctx.Done() parks forever for every attendant
// dismissed the blessed way. One per meeting, for the life of the process.
//
// Both arrivals are asserted, and the effect count is the second half of each: a
// watch that never started leaks nothing and also never disconnects, which is a
// different bug wearing this test's green.
func TestDismissRetiresItsWatch(t *testing.T) {
	const n = 50

	t.Run("dismissed by context", func(t *testing.T) {
		var mu sync.Mutex
		ran := 0
		over := left(func() {
			for range n {
				ctx, cancel := context.WithCancel(context.Background())
				dismissed(ctx, func() { mu.Lock(); ran++; mu.Unlock() })
				cancel()
			}
		})
		if over > 0 {
			t.Errorf("%d goroutines left after %d context dismissals", over, n)
		}
		mu.Lock()
		defer mu.Unlock()
		if ran != n {
			t.Errorf("the effect ran %d times, want %d — the watch never fired", ran, n)
		}
	})

	t.Run("dismissed by call, on a context nobody cancels", func(t *testing.T) {
		var mu sync.Mutex
		ran := 0
		// context.Background() is never cancelled, which is exactly the shape the
		// leak needs: nothing will ever wake a watch that only selects on Done.
		over := left(func() {
			for range n {
				d := dismissed(context.Background(), func() { mu.Lock(); ran++; mu.Unlock() })
				d.dismiss()
			}
		})
		if over > 0 {
			t.Errorf("%d goroutines left after %d call dismissals — the watch is parked forever", over, n)
		}
		mu.Lock()
		defer mu.Unlock()
		if ran != n {
			t.Errorf("the effect ran %d times, want %d", ran, n)
		}
	})
}

// TestDismissRunsOnce: cancellation and an explicit call race by design, and the
// effect is a disconnect — running it twice is a second disconnect on a room that
// has already gone.
func TestDismissRunsOnce(t *testing.T) {
	var ran int
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	d := dismissed(ctx, func() { mu.Lock(); ran++; mu.Unlock() })

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); d.dismiss() }()
	}
	cancel()
	wg.Wait()
	d.dismiss()

	mu.Lock()
	defer mu.Unlock()
	if ran != 1 {
		t.Fatalf("the effect ran %d times, want exactly 1", ran)
	}
}

// TestDismissHoldsNoLockOverItsEffect. Leave used to take the attendant's mutex
// and hold it across room.Disconnect, so one room whose disconnect blocks stalls
// every other dismissal — and dismissals arrive together, because a process
// shutting down dismisses every room it holds.
//
// The effect here blocks until released. If dismiss serialised on a shared lock,
// the second call could not return while the first is inside its effect.
func TestDismissHoldsNoLockOverItsEffect(t *testing.T) {
	block := make(chan struct{})
	slow := dismissed(context.Background(), func() { <-block })
	quick := dismissed(context.Background(), func() {})

	go slow.dismiss()
	time.Sleep(50 * time.Millisecond) // let the slow one get inside its effect

	done := make(chan struct{})
	go func() { quick.dismiss(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked disconnect stalled an unrelated dismissal")
	}
	close(block)
}
