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
	"math/rand/v2"
	"sort"
	"time"
)

// Stage is one segment of a load profile. The rate moves linearly from From to
// To (requests per second) over Duration, so From == To is a constant rate, a
// larger To is a ramp up, and zero throughout is a pause.
type Stage struct {
	Duration time.Duration
	From, To float64
}

// ConstantRate is the one-stage profile that holds rate for d.
func ConstantRate(rate float64, d time.Duration) []Stage {
	return []Stage{{Duration: d, From: rate, To: rate}}
}

// Arrivals is how requests are spread within the rate the profile asks for.
type Arrivals int

const (
	// Even spaces requests exactly 1/rate apart.
	Even Arrivals = iota
	// Poisson draws random exponential gaps with the same average rate. Real
	// traffic is bursty, and bursts queue: a server that copes with evenly
	// spaced requests at some rate can still build a queue at the same average
	// rate when they arrive in random clumps.
	Poisson
)

func (a Arrivals) String() string {
	switch a {
	case Even:
		return "even"
	case Poisson:
		return "poisson"
	}
	return fmt.Sprintf("Arrivals(%d)", int(a))
}

// segment is a Stage with what is needed to invert it.
type segment struct {
	offset time.Duration // where the stage starts, from the start of the schedule
	a, b   float64       // rate at the start (rps) and how fast it changes (rps per second)
	before float64       // expected number of requests due before this stage
	count  float64       // expected number of requests due within it
}

// Schedule produces the intended send time of each request, in order. It is
// not safe for concurrent use.
//
// Think of the profile's rate λ(t) and its running total Λ(t), the expected
// number of requests due by time t. The k-th request is due when Λ reaches
// some value m. For Even arrivals m = k; for Poisson, m is the k-th arrival of
// a unit-rate random process. Either way the time is found by inverting Λ
// directly, in closed form, for the constant or linearly changing rate of the
// stage it falls in, so nothing accumulates and nothing drifts: a rate such as
// 3/s, whose interval is not a whole number of nanoseconds, stays exact over
// millions of requests, and a fraction of a request left at the end of one
// stage carries into the next.
type Schedule struct {
	start    time.Time
	segs     []segment
	total    float64 // expected number of requests in the whole profile
	duration time.Duration

	arrivals Arrivals
	rng      *rand.Rand
	n        uint64  // requests produced so far (Even)
	m        float64 // position on the unit-rate process (Poisson)

	last time.Duration // offset of the previous request; due times never go back
	done bool
}

// New returns a schedule that starts at start. seed only matters for Poisson
// arrivals: the same seed gives the same arrival times.
func New(start time.Time, stages []Stage, arrivals Arrivals, seed uint64) (*Schedule, error) {
	if len(stages) == 0 {
		return nil, fmt.Errorf("pacer: the profile has no stages")
	}
	if arrivals != Even && arrivals != Poisson {
		return nil, fmt.Errorf("pacer: unknown arrival process %v", arrivals)
	}
	s := &Schedule{start: start, arrivals: arrivals}
	for i, st := range stages {
		if st.Duration <= 0 {
			return nil, fmt.Errorf("pacer: stage %d: duration must be positive, got %v", i+1, st.Duration)
		}
		for _, r := range []float64{st.From, st.To} {
			if r < 0 || math.IsNaN(r) || math.IsInf(r, 0) {
				return nil, fmt.Errorf("pacer: stage %d: rate must be a non-negative, finite number, got %v", i+1, r)
			}
		}
		sec := st.Duration.Seconds()
		seg := segment{
			offset: s.duration,
			a:      st.From,
			b:      (st.To - st.From) / sec,
			before: s.total,
			count:  (st.From + st.To) / 2 * sec,
		}
		s.segs = append(s.segs, seg)
		s.total += seg.count
		s.duration += st.Duration
	}
	if s.total <= 0 {
		return nil, fmt.Errorf("pacer: the profile schedules no requests: the rate must be positive for part of it")
	}
	if arrivals == Poisson {
		s.rng = rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	}
	return s, nil
}

// Duration is the length of the whole profile.
func (s *Schedule) Duration() time.Duration { return s.duration }

// Expected is the average number of requests the profile schedules.
func (s *Schedule) Expected() float64 { return s.total }

// Next returns when the next request is due and the index of the stage that
// time falls in. ok is false once the profile is over: the next request would
// be due at or after its end.
func (s *Schedule) Next() (at time.Time, stage int, ok bool) {
	if s.done {
		return time.Time{}, 0, false
	}
	var m float64
	if s.arrivals == Poisson {
		s.m += s.rng.ExpFloat64()
		m = s.m
	} else {
		m = float64(s.n)
		s.n++
	}

	i := sort.Search(len(s.segs), func(i int) bool { return s.segs[i].before+s.segs[i].count > m })
	if i == len(s.segs) {
		s.done = true
		return time.Time{}, 0, false
	}
	seg := s.segs[i]
	off := max(seg.offset+seg.timeAt(m-seg.before), s.last) // max: guard against a rounding step backwards
	if off >= s.duration {
		s.done = true
		return time.Time{}, 0, false
	}
	s.last = off
	return s.start.Add(off), i, true
}

// timeAt is how far into the stage the expected count reaches x, where x is
// how many requests are due within the stage so far: the root of
// a*t + b*t²/2 = x. It uses the form 2x / (a + sqrt(a² + 2bx)), which stays
// accurate when b is tiny and also when a is zero (a ramp up from nothing).
func (g segment) timeAt(x float64) time.Duration {
	if x <= 0 {
		return 0
	}
	if g.b == 0 {
		return time.Duration(x * 1e9 / g.a)
	}
	disc := max(g.a*g.a+2*g.b*x, 0)
	return time.Duration(2 * x * 1e9 / (g.a + math.Sqrt(disc)))
}
