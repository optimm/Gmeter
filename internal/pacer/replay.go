package pacer

import (
	"fmt"
	"slices"
	"time"
)

// Replay is a schedule whose due times were recorded rather than computed from
// a rate: request i is due offsets[i] after the start. It is what replaying a
// traffic log uses, and like Schedule it is open loop: the times are fixed on
// the clock and nothing about the responses can move them. It is not safe for
// concurrent use.
type Replay struct {
	start   time.Time
	offsets []time.Duration
	i       int
}

// NewReplay returns a schedule that starts at start and makes request i due at
// start+offsets[i]. The offsets must be non-negative and in order; equal
// offsets are fine (two requests recorded at the same instant).
func NewReplay(start time.Time, offsets []time.Duration) (*Replay, error) {
	if len(offsets) == 0 {
		return nil, fmt.Errorf("pacer: the replay has no requests")
	}
	for i, o := range offsets {
		if o < 0 {
			return nil, fmt.Errorf("pacer: request %d: offset must not be negative, got %v", i+1, o)
		}
		if i > 0 && o < offsets[i-1] {
			return nil, fmt.Errorf("pacer: request %d: offsets must be in order, but %v comes after %v", i+1, o, offsets[i-1])
		}
	}
	return &Replay{start: start, offsets: slices.Clone(offsets)}, nil
}

// Duration is how long the replay lasts: the offset of its last request.
func (r *Replay) Duration() time.Duration { return r.offsets[len(r.offsets)-1] }

// Len is the number of requests in the replay.
func (r *Replay) Len() int { return len(r.offsets) }

// Next returns when the next request is due. The stage is always 0: a replay is
// one stage. ok is false once every request has been returned.
func (r *Replay) Next() (at time.Time, stage int, ok bool) {
	if r.i >= len(r.offsets) {
		return time.Time{}, 0, false
	}
	at = r.start.Add(r.offsets[r.i])
	r.i++
	return at, 0, true
}
