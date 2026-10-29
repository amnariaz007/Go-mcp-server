package ratelimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBurstThenRateLimited(t *testing.T) {
	now := time.Unix(0, 0)
	l := newWithClock(10, 3, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if d := l.reserve(); d != 0 {
			t.Fatalf("burst token %d delayed %v", i, d)
		}
	}
	if d := l.reserve(); d != 100*time.Millisecond {
		t.Fatalf("4th token delay = %v, want 100ms", d)
	}
	if d := l.reserve(); d != 200*time.Millisecond {
		t.Fatalf("5th token delay = %v, want 200ms (reservations queue)", d)
	}
	now = now.Add(time.Second)
	if d := l.reserve(); d != 0 {
		t.Fatalf("after refill delay = %v", d)
	}
}

func TestConcurrentWaitersRespectRate(t *testing.T) {
	l := New(50, 1)
	var n atomic.Int32
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 11; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Wait(context.Background()); err == nil {
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	// 1 burst token + 10 tokens at 50/s needs ~200ms.
	if el := time.Since(start); el < 180*time.Millisecond {
		t.Fatalf("11 waits at 50 rps with burst 1 finished in %v; limiter not shared", el)
	}
	if n.Load() != 11 {
		t.Fatalf("completed %d", n.Load())
	}
}

func TestWaitHonoursCancellation(t *testing.T) {
	l := New(0.1, 1)
	_ = l.Wait(context.Background()) // drain
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	// The cancelled reservation is returned, so the next caller does not wait twice as long.
	if l.tokens < -0.01 {
		t.Fatalf("token debt not refunded: %v", l.tokens)
	}
}
