// Package pacer produces an open-loop send schedule.
//
// In an open-loop load test, request n is due at a time fixed in advance by the
// clock, independent of how quickly earlier requests were answered. Latency is
// then measured from that intended time, which is what avoids coordinated
// omission: if the server stalls, the requests that *should* have been sent
// during the stall still show up as slow instead of silently not existing.
package pacer

import (
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
