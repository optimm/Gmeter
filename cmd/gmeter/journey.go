package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/journey"
	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/report"
	"github.com/optimm/Gmeter/internal/runner"
)

// runJourney implements `gmeter journey -scenario FILE`. Exit code: 0 the
// test ran, 1 it failed or was interrupted, 2 bad usage or a bad scenario.
func runJourney(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gmeter journey", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		scenario = fs.String("scenario", "", "the journey scenario file: YAML with steps, checks and extraction (required)")
		rate     = fs.Float64("rate", 100, "sessions per second (open loop)")
		duration = fs.Duration("duration", 10*time.Second, "how long to keep scheduling sessions")
	)
	rf := addCommonFlags(fs, "even", 50)
	addPhasesFlag(fs, rf)
	addDialFlags(fs, rf)
	addPrometheusFlag(fs, rf)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gmeter journey -scenario FILE [flags] URL\n\n"+
			"Runs one session of the scenario per scheduled time. -rate is sessions per second,\n"+
			"not requests; the report's achieved rate is sessions/s, and the steps are the\n"+
			"journey's rows. Steps run in order within a session, all still due at their own\n"+
			"times: a step is charged from the previous step's finish plus its think, not from\n"+
			"the session's whole wait.\n\n")
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
		fmt.Fprintln(stderr, "gmeter journey: missing URL")
		fs.Usage()
		return 2
	case n > 1:
		fmt.Fprintf(stderr, "gmeter journey: expected exactly one URL but got %d arguments (flags must come before the URL)\n", n)
		return 2
	}
	target := fs.Arg(0)

	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "gmeter journey: "+format+"\n", a...)
		return 2
	}
	sc, err := journey.Compile(*scenario)
	if err != nil {
		return usage("-scenario: %v", err)
	}
	if *rate <= 0 {
		return usage("-rate must be positive, got %g", *rate)
	}
	if *duration <= 0 {
		return usage("-duration must be positive, got %v", *duration)
	}
	arrival, seed, err := rf.arrival()
	if err != nil {
		return usage("%v", err)
	}
	cfg, err := rf.base(target)
	if err != nil {
		return usage("%v", err)
	}
	cfg.Journey = sc
	cfg.Rate, cfg.Duration = *rate, *duration
	cfg.Stages = pacer.ConstantRate(*rate, *duration)
	cfg.Arrivals, cfg.Seed = arrival, seed

	shownURL := redactURL(cfg.URL) // anything printed or saved uses this, never cfg.URL
	scrub := func(s string) string { return strings.ReplaceAll(s, cfg.URL, shownURL) }
	if !*rf.quiet {
		fmt.Fprintf(stderr, "gmeter: journey %s %s at %g sessions/s for %v, up to %d sessions in flight\n",
			sc.Name, shownURL, *rate, *duration, cfg.Workers)
		cfg.Progress = progressTo(stderr)
	}
	rf.noteTLS(stderr)

	started := time.Now().UTC()
	res, code := rf.execute(ctx, cfg, nil, "gmeter journey", stderr, scrub)
	if res == nil {
		return code
	}

	summary := report.Summarize(res, *rate)
	summary.Run = report.RunInfo{
		URL:             shownURL,
		Method:          journeyMethods(sc),
		Workers:         cfg.Workers,
		DurationSeconds: duration.Seconds(),
		Profile:         profileSpec(cfg.Stages),
		Arrivals:        arrival.String(),
		Seed:            seed,
		TimeoutSeconds:  cfg.Timeout.Seconds(),
		SpinSeconds:     cfg.Spin.Seconds(),
		StartedAt:       started.Format(time.RFC3339),
		Protocol:        rf.protocol(),
		Journey: &report.JourneyInfo{
			Name:              sc.Name,
			Steps:             len(sc.Steps),
			SessionsStarted:   res.Scheduled,
			SessionsCompleted: res.Journey.SessionsCompleted,
			SessionsAborted:   res.Journey.SessionsAborted,
			Failures:          res.Journey.Failures,
		},
	}
	var prom string
	if rf.prometheus != nil {
		prom = *rf.prometheus
	}
	return emit(summary, *rf.jsonOut, prom, stdout, stderr)
}

// journeyMethods lists the journey's steps' methods, comma-separated, as the
// report's Method.
func journeyMethods(sc *runner.Journey) string {
	seen := map[string]bool{}
	for _, st := range sc.Steps {
		m := st.Method
		if m == "" {
			m = "GET"
		}
		seen[m] = true
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	if len(out) == 0 {
		return ""
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}
