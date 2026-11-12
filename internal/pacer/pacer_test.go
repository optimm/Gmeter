package pacer

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFirstRequestIsDueAtStart(t *testing.T) {
	s, err := New(t0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.At(0); !got.Equal(t0) {
		t.Fatalf("At(0) = %v, want %v", got, t0)
	}
}

func TestRequestsAreSpacedByTheRateInterval(t *testing.T) {
	s, _ := New(t0, 100) // one every 10ms
	if got, want := s.At(1), t0.Add(10*time.Millisecond); !got.Equal(want) {
		t.Fatalf("At(1) = %v, want %v", got, want)
	}
	if got, want := s.At(250), t0.Add(2500*time.Millisecond); !got.Equal(want) {
		t.Fatalf("At(250) = %v, want %v", got, want)
	}
}

func TestScheduleDoesNotDriftWhenRateDoesNotDivideASecond(t *testing.T) {
	// 3 rps => 333,333,333.33ns per request. Accumulating a truncated
	// interval would drift by 1ns per request; computing from the index must not.
	s, _ := New(t0, 3)
	if got, want := s.At(3), t0.Add(time.Second); !got.Equal(want) {
		t.Fatalf("At(3) = %v, want exactly %v", got, want)
	}
	const n = 3_000_000
	got := s.At(n).Sub(t0)
	want := 1_000_000 * time.Second
	if diff := (got - want).Abs(); diff > time.Nanosecond {
		t.Fatalf("At(%d) off by %v, want <=1ns (no accumulated drift)", n, diff)
	}
}

func TestInvalidRatesAreRejected(t *testing.T) {
	for _, rate := range []float64{0, -5, math.NaN(), math.Inf(1)} {
		if _, err := New(t0, rate); err == nil {
			t.Errorf("New(rate=%v) succeeded, want error", rate)
		}
	}
}

// spins are the pacing modes every behaviour below must hold for: a plain
// timer, a short spin, and a spin window longer than the whole wait (so the
// wait is a pure busy-wait).
var spins = []struct {
	name string
	spin time.Duration
}{
	{"timer only", 0},
	{"short spin", 200 * time.Microsecond},
	{"spin longer than the wait", time.Hour},
}

func TestWaitUntilReturnsImmediatelyForPastTimes(t *testing.T) {
	for _, m := range spins {
		t.Run(m.name, func(t *testing.T) {
			start := time.Now()
			if err := WaitUntil(context.Background(), start.Add(-time.Hour), m.spin); err != nil {
				t.Fatalf("WaitUntil(past) = %v, want nil", err)
			}
			if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
				t.Fatalf("WaitUntil(past) blocked for %v", elapsed)
			}
		})
	}
}

func TestWaitUntilNeverReturnsBeforeTheDeadline(t *testing.T) {
	for _, m := range spins {
		t.Run(m.name, func(t *testing.T) {
			for range 5 {
				deadline := time.Now().Add(30 * time.Millisecond)
				if err := WaitUntil(context.Background(), deadline, m.spin); err != nil {
					t.Fatal(err)
				}
				if early := time.Until(deadline); early > 0 {
					t.Fatalf("WaitUntil returned %v before the deadline", early)
				}
			}
		})
	}
}

func TestWaitUntilStopsWhenContextIsCancelled(t *testing.T) {
	// The far deadline puts a plain timer, and the start of a spinning wait,
	// in their sleeping phase; the huge spin window puts the whole wait in the
	// busy-wait phase. Cancellation must be noticed in each.
	for _, m := range spins {
		t.Run(m.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			start := time.Now()
			err := WaitUntil(ctx, start.Add(10*time.Second), m.spin)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("WaitUntil = %v, want context.Canceled", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("WaitUntil kept waiting %v after cancel", elapsed)
			}
		})
	}
}

// A timer wakes late, by an amount that depends on the OS: on one Mac the median
// was about 2ms for sleeps of 10ms or more. That lateness
// lands in every request's corrected latency, so spinning through the last
// stretch must remove it. Only the median is asserted: the tail depends on how
// busy the machine is, and no pacing strategy can fix that.
func TestWaitUntilWithSpinWakesCloseToTheDeadline(t *testing.T) {
	const waits = 40
	start := time.Now()
	lags := make([]time.Duration, waits)
	for i := range lags {
		due := start.Add(time.Duration(i+1) * 10 * time.Millisecond)
		if err := WaitUntil(context.Background(), due, 200*time.Microsecond); err != nil {
			t.Fatal(err)
		}
		lags[i] = time.Since(due)
		if lags[i] < 0 {
			t.Fatalf("wait %d returned %v early", i, -lags[i])
		}
	}
	slices.Sort(lags)
	if median := lags[waits/2]; median > 250*time.Microsecond {
		t.Fatalf("median wake-up lag with spinning = %v, want < 250µs (sorted lags: %v)", median, lags)
	}
}

// Spinning costs CPU in proportion to spin/gap, and it only pays off while the
// timer is coarse relative to the gap. At 5000 rps a plain timer is already
// within ~50µs, so spinning through a 200µs window would burn a whole core for
// nothing. The window is therefore capped at a quarter of the gap, and dropped
// altogether when that leaves less than minSleep: a window shorter than the
// shortest sleep cannot absorb a timer's overshoot, so the scheduler would
// never sleep and would just busy-loop the whole gap.
func TestCapSpinKeepsTheBusyWaitToAQuarterOfTheGap(t *testing.T) {
	cases := []struct {
		name      string
		spin, gap time.Duration
		want      time.Duration
	}{
		{"gap much longer than the window", 200 * time.Microsecond, 10 * time.Millisecond, 200 * time.Microsecond},
		{"gap exactly four windows", 200 * time.Microsecond, 800 * time.Microsecond, 200 * time.Microsecond},
		{"5000 rps: window is exactly minSleep", 200 * time.Microsecond, 200 * time.Microsecond, 50 * time.Microsecond},
		{"10000 rps: a 25µs window cannot cover a timer's overshoot", 200 * time.Microsecond, 100 * time.Microsecond, 0},
		{"20000 rps", 200 * time.Microsecond, 50 * time.Microsecond, 0},
		{"spinning disabled", 0, 10 * time.Millisecond, 0},
		{"no gap", 200 * time.Microsecond, 0, 0},
		{"negative gap", 200 * time.Microsecond, -time.Millisecond, 0},
	}
	for _, c := range cases {
		if got := CapSpin(c.spin, c.gap); got != c.want {
			t.Errorf("%s: CapSpin(%v, %v) = %v, want %v", c.name, c.spin, c.gap, got, c.want)
		}
	}
}

func TestCoarseSleepApproachesTheSpinWindowWithoutEnteringIt(t *testing.T) {
	const spin = 50 * time.Microsecond
	cases := []struct {
		name string
		rem  time.Duration
		want time.Duration
	}{
		{"far away: sleep half of the slack", 10 * time.Millisecond, (10*time.Millisecond - spin) / 2},
		{"slack of 150µs: still halved", 200 * time.Microsecond, 75 * time.Microsecond},
		{"slack under 2*minSleep: sleep all of it", 125 * time.Microsecond, 75 * time.Microsecond},
		{"slack of exactly minSleep", 100 * time.Microsecond, 50 * time.Microsecond},
		{"slack under minSleep: spin", 99 * time.Microsecond, 0},
		{"already inside the window", 30 * time.Microsecond, 0},
		{"deadline reached", 0, 0},
		{"deadline passed", -time.Millisecond, 0},
	}
	for _, c := range cases {
		if got := coarseSleep(c.rem, spin); got != c.want {
			t.Errorf("%s: coarseSleep(%v, %v) = %v, want %v", c.name, c.rem, spin, got, c.want)
		}
	}

	// Whatever the inputs, sleeping must never eat into the spin window: that
	// is what bounds the busy-wait to the window, and so the CPU cost.
	for _, spin := range []time.Duration{10 * time.Microsecond, 50 * time.Microsecond, 200 * time.Microsecond, time.Millisecond} {
		for rem := time.Duration(0); rem < 20*time.Millisecond; rem += 7 * time.Microsecond {
			if d := coarseSleep(rem, spin); d != 0 && rem-d < spin {
				t.Fatalf("coarseSleep(%v, %v) = %v leaves %v before the deadline, inside the %v window", rem, spin, d, rem-d, spin)
			}
		}
	}
}
