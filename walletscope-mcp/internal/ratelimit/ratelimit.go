// Package ratelimit provides a context-aware token-bucket limiter shared by all
// goroutines that call one upstream service.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter is a token bucket refilled at rate tokens per second up to burst.
type Limiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// New returns a limiter that starts full.
func New(ratePerSecond float64, burst int) *Limiter {
	return newWithClock(ratePerSecond, burst, time.Now)
}

func newWithClock(ratePerSecond float64, burst int, now func() time.Time) *Limiter {
	if burst < 1 {
		burst = 1
	}
	return &Limiter{rate: ratePerSecond, burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

// Wait blocks until a token is available or ctx is done. Tokens are reserved
// at call time, so concurrent waiters are served in a fair, bounded order and
// the long-run rate never exceeds the configured limit.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay := l.reserve()
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		l.cancelReservation()
		return ctx.Err()
	}
}

// reserve takes one token (possibly driving the balance negative) and returns
// how long the caller must wait before the reservation is honoured.
func (l *Limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	l.tokens--
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / l.rate * float64(time.Second))
}

func (l *Limiter) cancelReservation() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens++
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
}
