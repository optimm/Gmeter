package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimm/Gmeter"
)

// CapacityInput is what the find_capacity tool takes.
type CapacityInput struct {
	TargetInput
	SLO          string  `json:"slo" jsonschema:"the target to meet, as quantile=latency: p99=50ms, or p99.9=200ms"`
	MaxErrorRate float64 `json:"max_error_rate,omitempty" jsonschema:"the share of requests that may fail or get a bad status, as a fraction: 0.01 allows 1%; default none"`
	Start        float64 `json:"start,omitempty" jsonschema:"the first rate to try, requests per second; default 100"`
	Max          float64 `json:"max,omitempty" jsonschema:"never try more than this rate; default is the server's rate limit"`
	Trial        string  `json:"trial,omitempty" jsonschema:"how long each trial measures; default 10s"`
	Warmup       string  `json:"warmup,omitempty" jsonschema:"run first in each trial and not judged; default 2s, 0s for none"`
	Confirm      *int    `json:"confirm,omitempty" jsonschema:"how many times to repeat the answer at the end to see that it holds; default 2, 0 for none"`
}

// TrialOutput is one run of a capacity search, at one rate.
type TrialOutput struct {
	Phase      string   `json:"phase" jsonschema:"why it ran: grow, descend, bisect or confirm"`
	RPS        float64  `json:"rps"`
	Verdict    string   `json:"verdict" jsonschema:"pass, fail, or limited: it looked fine but the generator could not keep time, so it counts as a fail"`
	ObservedMs float64  `json:"observed_ms" jsonschema:"the corrected latency at the target's quantile"`
	ErrorRate  float64  `json:"error_rate"`
	Scheduled  uint64   `json:"scheduled"`
	Reasons    []string `json:"reasons" jsonschema:"why the trial failed or was not trusted; empty on a clean pass"`
}

// CapacityResult is what find_capacity returns.
type CapacityResult struct {
	Summary          string        `json:"summary" jsonschema:"the answer in one or two sentences"`
	Found            bool          `json:"found" jsonschema:"whether any rate met the target"`
	MaxRPS           float64       `json:"max_rps" jsonschema:"the highest rate that met the target"`
	NextFailRPS      float64       `json:"next_fail_rps" jsonschema:"the lowest rate above it that did not, 0 if none was tried"`
	LowerBound       bool          `json:"lower_bound" jsonschema:"the search stopped at its maximum: the service takes at least max_rps and may take more"`
	GeneratorLimited bool          `json:"generator_limited" jsonschema:"the failing rate could not be trusted because the generator did not keep time: the service may take more than max_rps"`
	Confirmed        int           `json:"confirmed" jsonschema:"how many of the confirm_runs repeats of max_rps passed"`
	ConfirmRuns      int           `json:"confirm_runs"`
	Trials           []TrialOutput `json:"trials"`
	Incomplete       bool          `json:"incomplete,omitempty" jsonschema:"the search was stopped before it finished, so this is what it had learned"`
}

const capacityDescription = `Find the highest request rate at which a service still meets a latency target.

It doubles the rate until a trial fails (halving it instead if the first one does), narrows the gap between
the highest pass and the lowest fail, then repeats the answer to see that it holds. Latency is measured from
each request's intended send time, so queueing in front of a slow server counts.

Give the target as quantile=latency, such as p99=50ms. Read the result as: max_rps is where it held;
lower_bound true means the search was stopped by its limit, not by the service, so it may take more;
generator_limited true means the failing rate was the generator's fault, not the service's. A search runs many
trials and takes minutes: keep trial short for a quick answer and longer for a trustworthy one.

Only loopback targets are allowed unless the operator started the server with -allow; the search never goes
above the server's rate limit, and a trial is never longer than its duration limit.`

// maxSearch bounds a whole search however many trials it runs.
const maxSearch = 15 * time.Minute

func (s *service) findCapacity(ctx context.Context, req *mcp.CallToolRequest, in CapacityInput) (*mcp.CallToolResult, CapacityResult, error) {
	cfg, err := s.target(ctx, in.TargetInput)
	if err != nil {
		return nil, CapacityResult{}, err
	}
	slo, err := parseSLO(in.SLO, in.MaxErrorRate)
	if err != nil {
		return nil, CapacityResult{}, err
	}
	opts := gmeter.SearchOptions{Start: in.Start, Max: in.Max}
	for _, d := range []struct {
		name, text string
		into       *time.Duration
	}{{"trial", in.Trial, &opts.Trial}, {"warmup", in.Warmup, &opts.Warmup}} {
		if d.text == "" {
			continue
		}
		v, err := time.ParseDuration(d.text)
		if err != nil || v < 0 {
			return nil, CapacityResult{}, fmt.Errorf("%s %q is not a duration such as 10s", d.name, d.text)
		}
		*d.into = v
		if d.name == "warmup" && v == 0 {
			*d.into = -1 // the package's way to say none
		}
	}
	if in.Confirm != nil {
		if *in.Confirm < 0 {
			return nil, CapacityResult{}, fmt.Errorf("confirm is a count of repeats, 0 for none")
		}
		opts.Confirm = *in.Confirm
		if *in.Confirm == 0 {
			opts.Confirm = -1
		}
	}

	// The search may not go above the operator's limits, and without a maximum of
	// its own it stops at them, not at the package's far higher default.
	if opts.Max == 0 {
		opts.Max = s.policy.MaxRate
	}
	trial := max(opts.Trial, 0)
	if trial == 0 {
		trial = 10 * time.Second
	}
	if err := s.policy.CheckLoad(max(opts.Max, opts.Start), max(trial, opts.Warmup), effectiveWorkers(cfg)); err != nil {
		return nil, CapacityResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, maxSearch)
	defer cancel()
	stop := reportProgress(ctx, req, "searching for capacity", 0)
	got, err := gmeter.FindCapacity(ctx, cfg, slo, opts)
	stop()
	incomplete := false
	if err != nil {
		if got == nil || (!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)) {
			return nil, CapacityResult{}, err
		}
		incomplete = true
	}
	out := capacityOf(got, in.SLO, opts.Max)
	out.Incomplete = incomplete
	if incomplete {
		out.Summary += " The search was stopped before it finished, so this is only what it had learned."
	}
	return nil, out, nil
}

func capacityOf(c *gmeter.Capacity, sloText string, searchMax float64) CapacityResult {
	out := CapacityResult{
		Found: c.Found, MaxRPS: c.MaxRPS, NextFailRPS: c.NextFailRPS, LowerBound: c.LowerBound,
		GeneratorLimited: c.GeneratorLimited, Confirmed: c.Confirmed, ConfirmRuns: c.ConfirmRuns,
		Trials: []TrialOutput{},
	}
	for _, t := range c.Trials {
		out.Trials = append(out.Trials, TrialOutput{
			Phase: t.Phase, RPS: t.RPS, Verdict: t.Verdict, ObservedMs: float64(t.Observed) / float64(time.Millisecond),
			ErrorRate: t.ErrorRate, Scheduled: t.Scheduled, Reasons: append([]string{}, t.Reasons...),
		})
	}

	var b strings.Builder
	switch {
	case !c.Found:
		why := ""
		if len(out.Trials) > 0 && len(out.Trials[0].Reasons) > 0 {
			why = " The first trial, at " + fmt.Sprintf("%.0f", out.Trials[0].RPS) + "/s, failed: " + strings.Join(out.Trials[0].Reasons, "; ") + "."
		}
		fmt.Fprintf(&b, "No rate met %s, down to the lowest one tried.%s", sloText, why)
	case c.LowerBound:
		fmt.Fprintf(&b, "The service holds at least %.0f/s within %s: the search stopped at its limit of %.0f/s, so it may take more.", c.MaxRPS, sloText, searchMax)
	default:
		fmt.Fprintf(&b, "The service holds %.0f/s within %s", c.MaxRPS, sloText)
		if c.NextFailRPS > 0 {
			fmt.Fprintf(&b, " and misses it at %.0f/s", c.NextFailRPS)
		}
		b.WriteString(".")
	}
	if c.Found && c.ConfirmRuns > 0 {
		fmt.Fprintf(&b, " Confirmed %d of %d repeats.", c.Confirmed, c.ConfirmRuns)
	}
	if c.GeneratorLimited {
		b.WriteString(" The failing rate was the load generator's limit, not the service's: a faster machine would show more.")
	}
	out.Summary = b.String()
	return out
}

// parseSLO reads a target written as "p99=50ms", with the errors it allows.
func parseSLO(text string, maxErrorRate float64) (gmeter.SLO, error) {
	if strings.TrimSpace(text) == "" {
		return gmeter.SLO{}, fmt.Errorf("slo is required, written as quantile=latency, such as p99=50ms")
	}
	slo, err := gmeter.ParseSLO(text)
	if err != nil {
		return slo, fmt.Errorf("slo %q: %w (write it as quantile=latency, such as p99=50ms)", text, err)
	}
	slo.MaxErrorRate = maxErrorRate
	return slo, nil
}
