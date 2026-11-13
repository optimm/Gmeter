package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/replay"
	"github.com/optimm/Gmeter/internal/report"
	"github.com/optimm/Gmeter/internal/runner"
)

// runReplay implements `gmeter replay`: send the requests of a traffic log to a
// server, either at the pace they were recorded at or at a fixed rate. Exit
// code: 0 ran (also when interrupted, with a partial report), 1 failed while
// writing the report, 2 bad usage, an unreadable trace, or a bad configuration.
func runReplay(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gmeter replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		file     = fs.String("file", "", "the traffic to replay: a JSON Lines trace or a common/combined access log (required)")
		format   = fs.String("format", "auto", "jsonl, clf (an access log) or auto (decided from the file's first line)")
		speed    = fs.Float64("speed", 1, "recorded timing only: replay this many times faster than recorded (2 = twice as fast, 0.5 = half)")
		rate     = fs.Float64("rate", 0, "send at this constant rate, in requests per second, instead of at the recorded times; the trace is cycled in order")
		duration = fs.Duration("duration", 0, "with -rate: how long to keep sending, cycling through the trace (default: send each request once)")
		methods  = fs.String("methods", "GET,HEAD", `which methods to replay, comma separated, or "all": a replay sends whatever the trace says, so the default leaves out requests that would write`)
		rows     = fs.Int("endpoints", 20, "the most rows in the per-endpoint breakdown: the busiest endpoints, and the rest together as (other); 0 leaves it out, and with it every path from the report and the saved JSON")
	)
	var groups groupFlag
	fs.Var(&groups, "group", `name the endpoint of the paths that match a rule, e.g. "/products/:slug" or "GET /static/**": * or :name stands for one segment, a last ** for the rest (repeatable; the first rule that matches wins, and paths no rule matches are grouped by their shape)`)
	rf := addCommonFlags(fs, "poisson", 50)
	addPhasesFlag(fs, rf)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gmeter replay -file TRACE [flags] BASE_URL\n\n"+
			"Sends the requests of a traffic log to BASE_URL (scheme, host and optional path prefix; the\n"+
			"log's paths are appended to it). By default each request is sent when it was recorded, so\n"+
			"the real mix and burstiness are reproduced; -rate sends them at a fixed rate instead.\n"+
			"Latency is measured from each request's intended send time. Flags must come before the URL.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "gmeter replay: "+format+"\n", a...)
		return 2
	}
	switch n := fs.NArg(); {
	case n == 0:
		fmt.Fprintln(stderr, "gmeter replay: missing URL")
		fs.Usage()
		return 2
	case n > 1:
		return usage("expected exactly one URL but got %d arguments (flags must come before the URL)", n)
	}
	target := fs.Arg(0)

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *file == "" {
		return usage("-file is required: the traffic to replay")
	}
	if !(*speed > 0) || math.IsInf(*speed, 0) {
		return usage("-speed must be a positive number, got %v", *speed)
	}
	fixedRate := set["rate"]
	switch {
	case fixedRate && !(*rate > 0):
		return usage("-rate must be positive, got %v", *rate)
	case fixedRate && set["speed"]:
		return usage("-speed only applies to recorded timing; with -rate the pace is the rate")
	case !fixedRate && set["duration"]:
		return usage("-duration only applies with -rate; recorded timing lasts as long as the recording")
	case fixedRate && set["duration"] && *duration <= 0:
		return usage("-duration must be positive, got %v", *duration)
	}
	if *rows == 0 && len(groups) > 0 {
		return usage("-group names endpoints, but -endpoints 0 leaves the endpoint breakdown out")
	}
	if *rows != 0 && (*rows < 2 || *rows > runner.MaxLabels) {
		return usage("-endpoints must be 0 (no breakdown) or between 2 and %d, got %d", runner.MaxLabels, *rows)
	}
	allowed, err := parseMethods(*methods)
	if err != nil {
		return usage("-methods: %v", err)
	}
	if *format != "auto" && *format != "jsonl" && *format != "clf" {
		return usage(`-format must be auto, jsonl or clf, got %q`, *format)
	}
	arrival, seed, err := rf.arrival()
	if err != nil {
		return usage("%v", err)
	}
	cfg, err := rf.base(target)
	if err != nil {
		return usage("%v", err)
	}
	shownURL := redactURL(cfg.URL) // anything printed or saved uses this, never cfg.URL
	scrub := func(s string) string { return strings.ReplaceAll(s, cfg.URL, shownURL) }

	trace, kind, err := loadTrace(*file, *format)
	if err != nil {
		return usage("%v", err)
	}
	skipped := 0
	if allowed != nil {
		skipped = trace.KeepMethods(allowed...)
	}
	if len(trace.Requests) == 0 {
		return usage("every request in %s has a method outside -methods (%s): nothing to replay; pass -methods all to include them", filepath.Base(*file), *methods)
	}
	if *rows > 0 {
		if err := trace.LabelEndpoints(*rows, groups...); err != nil { // after the filter: rank what will be sent
			return usage("-endpoints: %v", err)
		}
	}
	if !*rf.quiet {
		if trace.Unparsed > 0 {
			fmt.Fprintf(stderr, "gmeter: ignored %s of %s that %s not a usable request (garbage, a connection that sent nothing, or a proxy-style absolute URL)\n",
				count(trace.Unparsed, "line"), filepath.Base(*file), isAre(trace.Unparsed))
		}
		if skipped > 0 {
			fmt.Fprintf(stderr, "gmeter: skipped %s whose method is not one of %s: replaying them would send them to the server, writes included; pass -methods all, or a list, to include them\n",
				count(skipped, "request"), *methods)
		}
	}

	timing, runArrivals, runSeed := "fixed rate", arrival.String(), seed
	info := &report.ReplayInfo{Source: filepath.Base(*file), Format: kind, Requests: len(trace.Requests), Skipped: skipped, Unparsed: trace.Unparsed}
	if fixedRate {
		d := *duration
		if d == 0 {
			// One pass: the schedule ends exactly when the n-th request would be
			// due, computed the way the schedule computes it so n requests fit.
			d = time.Duration(float64(len(trace.Requests)) * 1e9 / *rate)
		}
		cfg.Stages, cfg.Arrivals, cfg.Seed = pacer.ConstantRate(*rate, d), arrival, seed
	} else {
		if !trace.Timed() {
			return usage("the trace has no times, so there is no pace to replay it at: give -rate to send it at a fixed rate")
		}
		if kind == "jsonl" {
			if set["arrivals"] || set["seed"] {
				return usage(`-arrivals and -seed have no effect on a trace with exact times (JSON Lines with "at")`)
			}
			runArrivals, runSeed = "recorded", 0 // the arrival times are the trace's own
		}
		offsets, err := trace.Offsets(*speed, arrival, seed)
		if err != nil {
			return usage("%v", err)
		}
		if offsets[len(offsets)-1] <= 0 {
			if len(offsets) == 1 {
				return usage("only 1 request is left, so there is no pace to replay it at: give -rate to send it at a fixed rate")
			}
			return usage("every request in the trace has the same time, so there is no pace to replay it at: give -rate to send it at a fixed rate")
		}
		cfg.Offsets = offsets
		timing, info.Speed = "recorded", *speed
	}
	info.Timing = timing
	cfg.Requests = trace.Requests

	if !*rf.quiet {
		how := fmt.Sprintf("recorded timing at %gx%s", *speed, spreadText(kind, arrival, seed))
		if fixedRate {
			how = strings.TrimPrefix(profileText(cfg.Stages), " ") + arrivalsText(arrival, seed)
		}
		fmt.Fprintf(stderr, "gmeter: replaying %s from %s against %s, %s, up to %d in flight\n",
			count(len(trace.Requests), "request"), filepath.Base(*file), shownURL, how, cfg.Workers)
		cfg.Progress = progressTo(stderr)
	}
	rf.noteTLS(stderr)

	started := time.Now().UTC()
	res, err := runner.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gmeter replay: %s\n", scrub(err.Error()))
		return 2
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "gmeter: interrupted; the report covers the partial run")
	}

	// The runner describes the replay as one stage: its length and average rate.
	stages := make([]pacer.Stage, len(res.Stages))
	for i, st := range res.Stages {
		stages[i] = st.Stage
	}
	totalDuration, expected := pacer.Totals(stages)
	summary := report.Summarize(res, targetRate(expected, totalDuration))
	// Only a fixed rate can send more than the trace holds: recorded timing sends each request once.
	if n := uint64(len(trace.Requests)); res.Scheduled > n {
		summary.Warnings = append(summary.Warnings, fmt.Sprintf(
			"the trace has %d requests but the run sent %d, so it was replayed %.1f times: every pass after the first is served from warmer caches, which flatters the latencies; use a trace at least as long as the run",
			n, res.Scheduled, float64(res.Scheduled)/float64(n)))
	}
	summary.Run = report.RunInfo{
		URL:             shownURL,
		Method:          methodsIn(trace.Requests),
		Workers:         cfg.Workers,
		DurationSeconds: totalDuration.Seconds(),
		Profile:         profileSpec(stages),
		Arrivals:        runArrivals,
		Seed:            runSeed,
		TimeoutSeconds:  cfg.Timeout.Seconds(),
		SpinSeconds:     cfg.Spin.Seconds(),
		StartedAt:       started.Format(time.RFC3339),
		Replay:          info,
	}
	return emit(summary, *rf.jsonOut, stdout, stderr)
}

// loadTrace reads the trace at path in the given format ("auto" decides from
// the first character: '{' is JSON Lines, anything else an access log). It
// returns the format it used.
func loadTrace(path, format string) (*replay.Trace, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	if format == "auto" {
		format = "jsonl"
		head, _ := br.Peek(4096)
		if first := strings.TrimSpace(string(head)); first != "" && first[0] != '{' {
			format = "clf"
		}
	}
	var tr *replay.Trace
	if format == "clf" {
		tr, err = replay.ParseCLF(br)
	} else {
		tr, err = replay.ParseJSONL(br)
	}
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return tr, format, nil
}

// parseMethods reads -methods. nil means every method.
func parseMethods(v string) ([]string, error) {
	if strings.EqualFold(strings.TrimSpace(v), "all") {
		return nil, nil
	}
	var out []string
	for _, m := range strings.Split(v, ",") {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m == "" || strings.ContainsFunc(m, func(c rune) bool { return c < 'A' || c > 'Z' }) {
			return nil, fmt.Errorf(`want a comma-separated list of HTTP methods such as GET,HEAD, or "all", got %q`, v)
		}
		out = append(out, m)
	}
	return out, nil
}

// methodsIn lists the distinct methods of the requests, sorted, e.g. "GET,POST".
// A request with no method is sent as GET.
func methodsIn(reqs []runner.Request) string {
	seen := map[string]bool{}
	for _, r := range reqs {
		m := r.Method
		if m == "" {
			m = "GET"
		}
		seen[m] = true
	}
	all := make([]string, 0, len(seen))
	for m := range seen {
		all = append(all, m)
	}
	slices.Sort(all)
	return strings.Join(all, ",")
}

// spreadText says how an access log's whole seconds are spread, for the banner.
// A trace with exact times needs no spreading.
func spreadText(kind string, a pacer.Arrivals, seed uint64) string {
	if kind != "clf" {
		return ""
	}
	if a == pacer.Poisson {
		return fmt.Sprintf(", requests spread randomly within each logged second (seed %d)", seed)
	}
	return ", requests spread evenly within each logged second"
}

func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// groupFlag collects -group rules, in the order given.
type groupFlag []replay.Group

func (g *groupFlag) String() string { return fmt.Sprint(len(*g), " rules") }

func (g *groupFlag) Set(v string) error {
	rule, err := replay.ParseGroup(v)
	if err != nil {
		return err
	}
	*g = append(*g, rule)
	return nil
}
