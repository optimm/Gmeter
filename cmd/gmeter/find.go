package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/find"
	"github.com/optimm/Gmeter/internal/report"
)

// runFind implements `gmeter find`: search for the highest request rate at
// which the target is still met. Exit code: 0 a rate was found (at least as
// high as reported), 1 no rate met the target or the search was interrupted or
// failed while running, 2 bad usage or configuration.
func runFind(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gmeter find", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		sloIn     = fs.String("slo", "", `the target a rate must meet: a latency quantile and its limit, e.g. "p99=50ms" or "p99.9=200ms" (required)`)
		maxErrors = fs.Float64("max-errors", 1, "the share of requests, in percent, that may fail or return a bad status")
		start     = fs.Float64("start", 100, "the first rate to try, in requests per second")
		maxRate   = fs.Float64("max", 100000, "never try more than this many requests per second")
		tolerance = fs.Float64("tolerance", 5, "stop narrowing when the passing and failing rates are within this many percent")
		trial     = fs.Duration("trial", 10*time.Second, "how long each trial measures")
		warmup    = fs.Duration("warmup", 2*time.Second, "how long each trial runs first, not judged, to take the cold-start cost (new connections, cold caches)")
		confirm   = fs.Int("confirm", 2, "how many times to repeat the answer at the end, to see whether it holds")
	)
	rf := addRequestFlags(fs, "poisson", 200)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gmeter find -slo p99=50ms [flags] URL\n\n"+
			"Finds the highest request rate at which the latency target is still met: it doubles the\n"+
			"rate until the target fails, narrows in between, and repeats the answer to check it.\n"+
			"Latency is measured from each request's intended send time. Flags must come before the URL.\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch n := fs.NArg(); {
	case n == 0:
		fmt.Fprintln(stderr, "gmeter find: missing URL")
		fs.Usage()
		return 2
	case n > 1:
		fmt.Fprintf(stderr, "gmeter find: expected exactly one URL but got %d arguments (flags must come before the URL)\n", n)
		return 2
	}
	target := fs.Arg(0)

	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "gmeter find: "+format+"\n", a...)
		return 2
	}
	if *sloIn == "" {
		return usage("-slo is required: the latency target a rate has to meet, e.g. -slo p99=50ms")
	}
	slo, err := find.ParseSLO(*sloIn)
	if err != nil {
		return usage("-slo: %v", err)
	}
	if *maxErrors < 0 || *maxErrors > 100 {
		return usage("-max-errors must be a percentage between 0 and 100, got %g", *maxErrors)
	}
	slo.MaxErrorRate = *maxErrors / 100
	if *trial <= 0 {
		return usage("-trial must be positive, got %v", *trial)
	}
	if *warmup < 0 {
		return usage("-warmup must not be negative, got %v", *warmup)
	}
	search := find.Config{Start: *start, Max: *maxRate, Floor: 1, Tolerance: *tolerance / 100, Confirm: *confirm}
	if err := search.Validate(); err != nil {
		return usage("%v", err)
	}
	arrival, seed, err := rf.arrival()
	if err != nil {
		return usage("%v", err)
	}
	cfg, err := rf.base(target)
	if err != nil {
		return usage("%v", err)
	}
	cfg.Arrivals = arrival
	shownURL := redactURL(cfg.URL) // anything printed or saved uses this, never cfg.URL
	scrub := func(s string) string { return strings.ReplaceAll(s, cfg.URL, shownURL) }

	if !*rf.quiet {
		fmt.Fprintf(stderr, "gmeter: finding the highest rate for %s %s that meets %s, errors ≤ %g%%%s\n",
			cfg.Method, shownURL, slo, *maxErrors, arrivalsText(arrival, seed))
	}

	started := time.Now().UTC()
	trials := 0
	probe := func(ctx context.Context, rate float64) (find.Verdict, any, error) {
		trials++
		if trials > 1 && settle > 0 { // let the last trial's aftermath die down
			select {
			case <-time.After(settle):
			case <-ctx.Done():
				return find.Fail, nil, ctx.Err()
			}
		}
		v, o, err := find.RunTrial(ctx, cfg, slo, *warmup, *trial, seed+uint64(trials), rate)
		if err != nil {
			return v, nil, err
		}
		if !*rf.quiet {
			fmt.Fprintf(stderr, "[trial %d] %g rps: %s %s, errors %.1f%% -> %s\n",
				trials, rate, slo.Label(), report.FormatDuration(o.Observed), o.ErrorRate*100, v)
		}
		return v, o, nil
	}

	out, err := find.Search(ctx, search, probe)
	interrupted := ctx.Err() != nil
	if err != nil && !interrupted {
		fmt.Fprintf(stderr, "gmeter find: %s\n", scrub(err.Error()))
		if len(out.Trials) == 0 {
			return 2 // it never got as far as a trial: the request or its setup is wrong
		}
		return 1
	}

	summary := find.NewSummary(out, find.RunInfo{
		URL:            shownURL,
		Method:         cfg.Method,
		Workers:        cfg.Workers,
		TrialSeconds:   trial.Seconds(),
		WarmupSeconds:  warmup.Seconds(),
		TimeoutSeconds: cfg.Timeout.Seconds(),
		SpinSeconds:    cfg.Spin.Seconds(),
		Arrivals:       arrival.String(),
		Seed:           seed,
		StartedAt:      started.Format(time.RFC3339),
	}, slo, interrupted)

	if *rf.jsonOut == "-" {
		err = summary.WriteJSON(stdout)
	} else {
		err = summary.WriteText(stdout)
		if err == nil && *rf.jsonOut != "" {
			err = writeJSONTo(*rf.jsonOut, summary.WriteJSON)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "gmeter find: writing the report: %v\n", err)
		return 1
	}
	if interrupted || !summary.Result.Found {
		return 1
	}
	return 0
}
