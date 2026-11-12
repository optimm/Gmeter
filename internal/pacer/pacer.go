// Package pacer produces an open-loop send schedule.
//
// In an open-loop load test, request n is due at a time fixed in advance by the
// clock, independent of how quickly earlier requests were answered. Latency is
// then measured from that intended time, which is what avoids coordinated
// omission: if the server stalls, the requests that *should* have been sent
// during the stall still show up as slow instead of silently not existing.
package pacer

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Schedule maps a request sequence number to its intended send time.
type Schedule struct {
	start time.Time
	rate  float64 // requests per second
}

// New returns a constant-rate schedule whose request 0 is due at start.
func New(start time.Time, rate float64) (Schedule, error) {
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return Schedule{}, fmt.Errorf("pacer: rate must be a positive, finite number, got %v", rate)
	}
	return Schedule{start: start, rate: rate}, nil
}

// At returns the intended send time of request n.
//
// It is computed from n directly rather than by accumulating an interval, so
// rounding error never builds up over millions of requests (a rate such as
// 3/s has a non-integer nanosecond interval that would otherwise drift).
func (s Schedule) At(n uint64) time.Time {
	return s.start.Add(time.Duration(float64(n) * 1e9 / s.rate))
}

// SleepUntil blocks until t or until ctx is done, whichever comes first. It
// returns nil if t was reached (or already in the past) and ctx.Err()
// otherwise. A past deadline returns immediately without yielding, so a
// generator that has fallen behind catches up by sending back-to-back.
func SleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
