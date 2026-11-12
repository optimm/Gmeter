// Package replay reads a traffic log into the requests to send and, when the
// log recorded times, when each one was due.
//
// Two formats are understood: JSON Lines written for this tool (exact times,
// any method, headers and body) and the common/combined access-log format that
// nginx, Apache and most proxies write (second-resolution times, no bodies).
package replay

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

// Trace is a log read into requests, in time order.
type Trace struct {
	Requests []runner.Request

	// Unparsed is how many lines of an access log were not requests Gmeter could
	// use (garbage, a connection that never sent a request, a proxy-style
	// absolute URL). They are counted rather than failing the whole log: real
	// access logs always have a few.
	Unparsed int

	stamps     []time.Duration // when each request was recorded, parallel to Requests; nil if the log has no times
	resolution time.Duration   // how coarse the stamps are: 0 exact, time.Second for an access log
}

// Timed reports whether the log recorded when each request was made, which is
// what replaying it at its own pace needs.
func (t *Trace) Timed() bool { return t.stamps != nil }

// Offsets returns when each request is due, counted from the first. speed
// scales the pace: 2 sends everything twice as fast as it was recorded, 0.5
// half as fast.
//
// An access log only records the second a request finished in, so what is known
// is how many requests there were in each second, not where inside it. They are
// spread across the second according to arrivals: evenly (the same count,
// exactly spaced) or as Poisson (the same count at random points, which is what
// arrivals of independent clients look like given only their count) using seed
// to make it repeatable. Exact times (JSON Lines) are used as they are, and
// arrivals and seed then have no effect.
func (t *Trace) Offsets(speed float64, arrivals pacer.Arrivals, seed uint64) ([]time.Duration, error) {
	if !t.Timed() {
		return nil, fmt.Errorf("the trace has no times to replay: give every request an \"at\" or use a fixed rate instead")
	}
	if !(speed > 0) || math.IsInf(speed, 0) {
		return nil, fmt.Errorf("speed must be a positive number, got %v", speed)
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	out := make([]time.Duration, len(t.stamps))
	base := t.stamps[0] // stamps are in order
	for i := 0; i < len(t.stamps); {
		j := i
		for j < len(t.stamps) && t.stamps[j] == t.stamps[i] {
			j++
		}
		// Requests i..j-1 share one stamp. With exact times that is a tie, and
		// they stay together; with coarse ones it is a bucket to spread them in.
		within := make([]time.Duration, j-i)
		if t.resolution > 0 {
			within = spread(len(within), t.resolution, arrivals, rng)
		}
		for k := i; k < j; k++ {
			out[k] = t.stamps[k] - base + within[k-i]
		}
		i = j
	}
	for i, o := range out {
		out[i] = time.Duration(float64(o) / speed)
	}
	return out, nil
}

// spread places n requests inside a window of length width, in order.
func spread(n int, width time.Duration, arrivals pacer.Arrivals, rng *rand.Rand) []time.Duration {
	at := make([]time.Duration, n)
	for i := range at {
		if arrivals == pacer.Poisson {
			at[i] = time.Duration(rng.Float64() * float64(width))
		} else {
			at[i] = time.Duration(float64(width) * float64(i) / float64(n))
		}
	}
	slices.Sort(at) // already sorted for even spacing
	return at
}

// KeepMethods drops every request whose method is not one of methods (a
// request with no method counts as GET, which is what it will be sent as) and
// returns how many it dropped. A replay performs whatever the log says, so a
// log with POSTs and DELETEs would write to the server under test; this is how
// a caller keeps to the safe ones. The times of the dropped requests go with
// them.
func (t *Trace) KeepMethods(methods ...string) (dropped int) {
	keep := map[string]bool{}
	for _, m := range methods {
		keep[strings.ToUpper(m)] = true
	}
	reqs := t.Requests[:0:0]
	var stamps []time.Duration
	for i, r := range t.Requests {
		m := r.Method
		if m == "" {
			m = "GET"
		}
		if !keep[m] {
			dropped++
			continue
		}
		reqs = append(reqs, r)
		if t.stamps != nil {
			stamps = append(stamps, t.stamps[i])
		}
	}
	t.Requests = reqs
	if t.stamps != nil {
		t.stamps = stamps
	}
	return dropped
}

// sortByTime puts the requests in time order, ties in the order they were read.
func (t *Trace) sortByTime() {
	if t.stamps == nil {
		return
	}
	order := make([]int, len(t.stamps))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return t.stamps[order[a]] < t.stamps[order[b]] })
	reqs := make([]runner.Request, len(order))
	stamps := make([]time.Duration, len(order))
	for i, from := range order {
		reqs[i], stamps[i] = t.Requests[from], t.stamps[from]
	}
	t.Requests, t.stamps = reqs, stamps
}
