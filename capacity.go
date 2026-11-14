package gmeter

import (
	"context"
	"fmt"
	"time"

	"github.com/optimm/Gmeter/internal/find"
)

// SearchOptions steers [FindCapacity]. Zero values take the defaults, which
// are the gmeter find command's.
type SearchOptions struct {
	Start     float64       // the first rate to try, requests per second; default 100
	Max       float64       // never try more than this; default 100000
	Trial     time.Duration // how long each trial measures; default 10s
	Warmup    time.Duration // run first in each trial and not judged, for connections and caches to warm; default 2s, negative for none
	Tolerance float64       // stop narrowing when the passing and failing rates are within this fraction of the passing one; default 0.05
	Confirm   int           // how many times to repeat the answer at the end; default 2, negative for none
	Settle    time.Duration // pause between trials, so one's aftermath does not leak into the next; default none
}

// CapacityTrial is one run at one rate.
type CapacityTrial struct {
	Phase     string        // why it ran: "grow", "descend", "bisect" or "confirm"
	RPS       float64       // the rate
	Verdict   string        // "pass", "fail", or "limited": it looked fine but the generator could not keep time, so it counts as a fail
	Observed  time.Duration // the latency at the target's quantile
	ErrorRate float64
	Scheduled uint64
	Reasons   []string // why it failed or was not trusted; empty on a clean pass
}

// Capacity is what [FindCapacity] learned.
type Capacity struct {
	// Found is whether any rate met the target. MaxRPS is the highest that did and
	// NextFailRPS the lowest rate above it that did not, 0 if none was tried.
	Found       bool
	MaxRPS      float64
	NextFailRPS float64
	// LowerBound means the search stopped at SearchOptions.Max, so the server can
	// take at least MaxRPS and may take more.
	LowerBound bool
	// GeneratorLimited means the lowest failing rate could not be trusted because
	// the generator did not keep time: the server may take more than MaxRPS, and a
	// faster machine or fewer other processes would show how much.
	GeneratorLimited bool
	// Confirmed of ConfirmRuns repeats of MaxRPS passed.
	Confirmed, ConfirmRuns int
	Trials                 []CapacityTrial
}

// FindCapacity finds the highest request rate at which the server still meets the
// target. It doubles the rate until a trial fails (halving it instead if the first
// one does), narrows the gap between the highest pass and the lowest fail, and
// repeats the answer to see whether it holds. Latency is measured from each
// request's due time, so queueing in front of a slow server counts.
//
// cfg says what to send and leaves the load empty: the search chooses the rates. If
// ctx is cancelled the search stops with ctx's error, and the Capacity returned
// holds what it had learned.
func FindCapacity(ctx context.Context, cfg Config, slo SLO, opts SearchOptions) (*Capacity, error) {
	target, err := slo.internal()
	if err != nil {
		return nil, err
	}
	base, closeCalls, err := cfg.runnerConfig(ctx, false)
	if err != nil {
		return nil, err
	}
	defer closeCalls()
	base.Phases = false
	search := find.Config{
		Start:     cmpOr(opts.Start, 100),
		Max:       cmpOr(opts.Max, 100000),
		Floor:     1,
		Tolerance: cmpOr(opts.Tolerance, 0.05),
		Confirm:   cmpOr(opts.Confirm, 2),
	}
	if opts.Confirm < 0 {
		search.Confirm = 0
	}
	trial, warmup := cmpOr(opts.Trial, 10*time.Second), cmpOr(opts.Warmup, 2*time.Second)
	if opts.Warmup < 0 {
		warmup = 0
	}
	if err := search.Validate(); err != nil {
		return nil, explain(err)
	}
	if trial < 0 {
		return nil, fmt.Errorf("gmeter: SearchOptions.Trial must be positive, got %v", trial)
	}

	trials := 0
	probe := func(ctx context.Context, rate float64) (find.Verdict, any, error) {
		trials++
		if trials > 1 && opts.Settle > 0 {
			select {
			case <-time.After(opts.Settle):
			case <-ctx.Done():
				return find.Fail, nil, ctx.Err()
			}
		}
		return find.RunTrial(ctx, base, target, warmup, trial, base.Seed+uint64(trials), rate)
	}
	out, err := find.Search(ctx, search, probe)

	c := &Capacity{
		Found:            out.Best > 0,
		MaxRPS:           out.Best,
		NextFailRPS:      out.NextFail,
		LowerBound:       out.LowerBound,
		GeneratorLimited: out.GeneratorLimited,
		Confirmed:        out.Confirmed,
		ConfirmRuns:      out.ConfirmRuns,
	}
	for _, t := range out.Trials {
		ct := CapacityTrial{Phase: string(t.Phase), RPS: t.Rate, Verdict: t.Verdict.String()}
		if o, ok := t.Detail.(find.Observation); ok {
			ct.Observed, ct.ErrorRate, ct.Scheduled, ct.Reasons = o.Observed, o.ErrorRate, o.Scheduled, o.Reasons
		}
		c.Trials = append(c.Trials, ct)
	}
	return c, explain(err)
}
