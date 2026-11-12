// Package find searches for the highest request rate a server sustains while
// still meeting a latency target.
//
// The search is blind to what a trial measures: it is given a Probe that runs
// one trial at a rate and says whether the target was met. That keeps the
// strategy, which is what is subtle here, separate from the load generation,
// and testable against fake servers of known capacity.
//
// It assumes what load testing always assumes, that latency does not improve
// as the rate goes up. Real servers are noisy, so the answer is only as good
// as the trials behind it; that is what the confirmation runs are for.
package find

import (
	"context"
	"fmt"
	"strconv"
)

// Verdict is the result of one trial.
type Verdict int

const (
	// Pass: the target was met.
	Pass Verdict = iota
	// Fail: the target was missed.
	Fail
	// Limited: the target looked met, but the load generator could not keep
	// time, so the trial says nothing reliable about the server. The search
	// treats it as a failure, and the outcome records that the generator may be
	// what stopped the search.
	Limited
)

// Phase says why a trial was run.
type Phase string

const (
	Grow    Phase = "grow"    // doubling the rate until the target fails
	Descend Phase = "descend" // halving it, because the starting rate already failed
	Bisect  Phase = "bisect"  // narrowing the gap between the highest pass and the lowest fail
	Confirm Phase = "confirm" // repeating the answer, to see whether it holds
)

// Trial is one run at one rate.
type Trial struct {
	Phase   Phase
	Rate    float64
	Verdict Verdict
	Detail  any // whatever the Probe attached, for the caller to report
}

// Probe runs one trial at rate. A non-nil error ends the search.
type Probe func(ctx context.Context, rate float64) (Verdict, any, error)

// Config controls the search. Rates are requests per second.
type Config struct {
	Start     float64 // the first rate to try
	Max       float64 // never try more than this
	Floor     float64 // never try less than this when descending (default 1)
	Tolerance float64 // stop bisecting when the gap is at most this fraction of the passing rate, e.g. 0.05
	Confirm   int     // how many times to repeat the answer at the end
}

// Outcome is what the search learned. It is valid, if partial, when Search
// returns an error.
type Outcome struct {
	Trials []Trial

	// Best is the highest rate that passed (0 if none did) and NextFail the
	// lowest rate above it that did not (0 if the search ended without one).
	Best, NextFail float64

	// LowerBound is set when Best is Config.Max: the search stopped there
	// because it was told to, not because the server gave out.
	LowerBound bool

	// ConfirmRuns trials repeated Best at the end, and Confirmed of them passed.
	Confirmed, ConfirmRuns int

	// GeneratorLimited is set when the lowest failing rate was a Limited trial:
	// the load generator, not the server, may be the real limit.
	GeneratorLimited bool
}

// Validate reports whether c can be searched with.
func (c Config) Validate() error { return c.validate() }

func (c *Config) validate() error {
	switch {
	case c.Start <= 0:
		return fmt.Errorf("find: the starting rate must be positive, got %v", c.Start)
	case c.Max < c.Start:
		return fmt.Errorf("find: the maximum rate (%v) must be at least the starting rate (%v)", c.Max, c.Start)
	case c.Floor < 0 || c.Floor > c.Max:
		return fmt.Errorf("find: the floor rate must be between 0 and the maximum, got %v", c.Floor)
	case c.Tolerance <= 0 || c.Tolerance > 1:
		return fmt.Errorf("find: the tolerance must be a fraction above 0 and at most 1, got %v", c.Tolerance)
	case c.Confirm < 0:
		return fmt.Errorf("find: confirmations must not be negative, got %d", c.Confirm)
	}
	if c.Floor == 0 {
		c.Floor = 1
	}
	return nil
}

// Search finds the highest rate that passes: it doubles the rate until a trial
// fails (or Max passes), halves it instead if the first trial fails, narrows
// the gap between the highest pass and the lowest fail until it is within
// Tolerance, and finally repeats the answer Confirm times.
func Search(ctx context.Context, cfg Config, probe Probe) (Outcome, error) {
	if err := cfg.validate(); err != nil {
		return Outcome{}, err
	}
	var o Outcome
	run := func(phase Phase, rate float64) (Verdict, error) {
		if err := ctx.Err(); err != nil {
			return Fail, err
		}
		v, detail, err := probe(ctx, rate)
		if err != nil {
			return v, err
		}
		o.Trials = append(o.Trials, Trial{Phase: phase, Rate: rate, Verdict: v, Detail: detail})
		return v, nil
	}
	pass := func(rate float64) { o.Best = rate }
	fail := func(rate float64, v Verdict) {
		o.NextFail = rate
		o.GeneratorLimited = v == Limited
	}

	rate, phase := min(max(cfg.Start, cfg.Floor), cfg.Max), Grow
	for {
		v, err := run(phase, rate)
		if err != nil {
			return o, err
		}
		if v == Pass {
			pass(rate)
			if rate >= cfg.Max {
				o.LowerBound = true
				break
			}
			if o.NextFail != 0 { // descending, and now something passes
				break
			}
			rate = min(rate*2, cfg.Max)
			continue
		}
		fail(rate, v)
		if o.Best > 0 || rate <= cfg.Floor { // a bracket, or nothing passes even at the floor
			break
		}
		phase, rate = Descend, max(rate/2, cfg.Floor)
	}

	for o.Best > 0 && o.NextFail > 0 && (o.NextFail-o.Best)/o.Best > cfg.Tolerance {
		mid := threeSignificantDigits((o.Best + o.NextFail) / 2)
		if mid <= o.Best || mid >= o.NextFail { // the bracket is finer than a readable rate
			break
		}
		v, err := run(Bisect, mid)
		if err != nil {
			return o, err
		}
		if v == Pass {
			pass(mid)
		} else {
			fail(mid, v)
		}
	}

	if o.Best > 0 {
		for range cfg.Confirm {
			v, err := run(Confirm, o.Best)
			if err != nil {
				return o, err
			}
			o.ConfirmRuns++
			if v == Pass {
				o.Confirmed++
			}
		}
	}
	return o, nil
}

// threeSignificantDigits rounds a rate to something readable (732.4531 rps
// becomes 732): a load level is not known to better than that anyway.
func threeSignificantDigits(x float64) float64 {
	r, err := strconv.ParseFloat(strconv.FormatFloat(x, 'g', 3, 64), 64)
	if err != nil {
		return x
	}
	return r
}
