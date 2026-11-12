// Command gmeter is an open-loop HTTP load generator that reports latency
// measured from each request's intended send time, so a stalled server shows
// up as slow instead of silently throttling the test (coordinated omission).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/report"
	"github.com/optimm/Gmeter/internal/runner"
)

// sampleEvery is a variable so tests can shorten it.
var sampleEvery = time.Second

// settle is the pause between the trials of `gmeter find`, so that one trial's
// aftermath (a backlog draining, a server recovering) does not leak into the
// next. A variable so tests can remove it.
var settle = time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process-global parts. It returns the exit code:
// 0 success, 2 bad usage or configuration, 1 failure while running.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "diff" { // a URL always has a scheme, so this cannot be one
		return runDiff(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "find" {
		return runFind(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "replay" {
		return runReplay(ctx, args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("gmeter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		rate     = fs.Float64("rate", 100, "target requests per second (open loop)")
		duration = fs.Duration("duration", 10*time.Second, "how long to keep scheduling requests")
		stagesIn = fs.String("stages", "", `load profile instead of -rate and -duration: comma-separated DURATION@RATE (constant) or DURATION@FROM->TO (ramp), e.g. "30s@100,30s@200" or "10s@50,1m@50->500"`)
	)
	rf := addRequestFlags(fs, "even", 50)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gmeter [flags] URL\n\nFlags must come before the URL.\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // the flag package has already printed the problem and the usage
	}
	switch n := fs.NArg(); {
	case n == 0:
		fmt.Fprintln(stderr, "gmeter: missing URL")
		fs.Usage()
		return 2
	case n > 1:
		fmt.Fprintf(stderr, "gmeter: expected exactly one URL but got %d arguments (flags must come before the URL)\n", n)
		return 2
	}
	target := fs.Arg(0)

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var stages []pacer.Stage
	if set["stages"] {
		if set["rate"] || set["duration"] {
			fmt.Fprintln(stderr, "gmeter: -stages cannot be combined with -rate or -duration: the stages already say how fast and for how long")
			return 2
		}
		var err error
		if stages, err = pacer.ParseStages(*stagesIn); err != nil {
			fmt.Fprintf(stderr, "gmeter: -stages: %v\n", err)
			return 2
		}
	} else {
		stages = pacer.ConstantRate(*rate, *duration)
	}
	arrival, seed, err := rf.arrival()
	if err != nil {
		fmt.Fprintf(stderr, "gmeter: %v\n", err)
		return 2
	}
	totalDuration, expected := pacer.Totals(stages)

	cfg, err := rf.base(target)
	if err != nil {
		fmt.Fprintf(stderr, "gmeter: %v\n", err)
		return 2
	}
	cfg.Stages, cfg.Arrivals, cfg.Seed = stages, arrival, seed
	shownURL := redactURL(cfg.URL) // anything printed or saved uses this, never cfg.URL
	if !*rf.quiet {
		fmt.Fprintf(stderr, "gmeter: %s %s%s%s, up to %d in flight\n",
			cfg.Method, shownURL, profileText(stages), arrivalsText(arrival, seed), cfg.Workers)
		cfg.Progress = progressTo(stderr)
	}

	started := time.Now().UTC()
	res, err := runner.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gmeter: %s\n", strings.ReplaceAll(err.Error(), cfg.URL, shownURL))
		return 2
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "gmeter: interrupted; the report covers the partial run")
	}

	summary := report.Summarize(res, targetRate(expected, totalDuration))
	summary.Run = report.RunInfo{
		URL:             shownURL,
		Method:          cfg.Method,
		Workers:         cfg.Workers,
		DurationSeconds: totalDuration.Seconds(),
		Profile:         profileSpec(stages),
		Arrivals:        arrival.String(),
		Seed:            seed,
		TimeoutSeconds:  cfg.Timeout.Seconds(),
		SpinSeconds:     cfg.Spin.Seconds(),
		StartedAt:       started.Format(time.RFC3339),
	}
	return emit(summary, *rf.jsonOut, stdout, stderr)
}

// progressTo returns the live progress line printer for a run.
func progressTo(stderr io.Writer) func(runner.Snapshot) {
	return func(s runner.Snapshot) {
		fmt.Fprintf(stderr, "[%5.1fs] %d scheduled, %d completed, %d failed, %d late | %s\n",
			s.Elapsed.Seconds(), s.Scheduled, s.Completed, s.Failed, s.Late, windowText(s.Window))
	}
}

// emit writes the finished run's report as -json asks: the text report (and a
// JSON file too if a path was given), or only the JSON on stdout for "-". It
// returns the exit code.
func emit(summary report.Summary, jsonOut string, stdout, stderr io.Writer) int {
	if jsonOut == "-" {
		if err := summary.WriteJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "gmeter: writing JSON: %v\n", err)
			return 1
		}
		return 0
	}
	if err := summary.WriteText(stdout); err != nil {
		fmt.Fprintf(stderr, "gmeter: writing report: %v\n", err)
		return 1
	}
	if jsonOut != "" {
		if err := writeJSONFile(jsonOut, summary); err != nil {
			fmt.Fprintf(stderr, "gmeter: writing %s: %v\n", jsonOut, err)
			return 1
		}
	}
	return 0
}

// profileText describes a load profile for the startup banner: " at 200 rps
// for 5s" for a constant rate, " with stages 30s@100,30s@200" otherwise.
func profileText(stages []pacer.Stage) string {
	if len(stages) == 1 && stages[0].From == stages[0].To {
		return fmt.Sprintf(" at %g rps for %v", stages[0].From, stages[0].Duration)
	}
	return " with stages " + profileSpec(stages)
}

// arrivalsText names the arrival process in the banner when it is not the
// default, with the seed that reproduces it.
func arrivalsText(a pacer.Arrivals, seed uint64) string {
	if a != pacer.Poisson {
		return ""
	}
	return fmt.Sprintf(" (poisson arrivals, seed %d)", seed)
}

// profileSpec is the profile in the notation -stages reads.
func profileSpec(stages []pacer.Stage) string {
	parts := make([]string, len(stages))
	for i, st := range stages {
		parts[i] = st.String()
	}
	return strings.Join(parts, ",")
}

// targetRate is the average rate of a profile.
func targetRate(expected float64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return expected / d.Seconds()
}

// redactURL hides a password embedded in the URL (user:pass@host). It is what
// gets printed and saved: a load-test summary is the kind of file that ends up
// in a CI log, a ticket or a repo. Query strings are kept as given, because
// they are often part of what makes two runs comparable.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	return u.Redacted()
}

// runDiff implements `gmeter diff [-threshold PERCENT] A.json B.json`. Exit
// code: 0 compared (and within the threshold, if one was given), 1 the
// threshold was exceeded, 2 bad usage or an unreadable file.
func runDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gmeter diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	threshold := fs.Float64("threshold", 0, "exit 1 if B's corrected p99 is more than this many percent above A's")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gmeter diff [-threshold PERCENT] A.json B.json\n\n"+
			"Compares two summaries saved with -json. Flags must come before the files.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	gated := false
	fs.Visit(func(f *flag.Flag) { gated = gated || f.Name == "threshold" }) // 0 is a meaningful threshold, so detect "was it given"
	if fs.NArg() != 2 {
		fmt.Fprintf(stderr, "gmeter diff: need exactly two summary files, got %d\n", fs.NArg())
		fs.Usage()
		return 2
	}

	var runs [2]report.Summary
	for i := range runs {
		s, err := loadSummary(fs.Arg(i))
		if err != nil {
			fmt.Fprintf(stderr, "gmeter diff: %v\n", err)
			return 2
		}
		runs[i] = s
	}
	cmp, err := report.Compare(runs[0], runs[1])
	if err != nil {
		fmt.Fprintf(stderr, "gmeter diff: %v\n", err)
		return 2
	}
	if err := cmp.WriteText(stdout, fs.Arg(0), fs.Arg(1)); err != nil {
		fmt.Fprintf(stderr, "gmeter diff: writing report: %v\n", err)
		return 1
	}

	if gated {
		change := p99Change(cmp)
		if cmp.Regressed(*threshold) {
			fmt.Fprintf(stdout, "\nREGRESSION: corrected p99 changed by %s, over the %g%% threshold\n", change, *threshold)
			return 1
		}
		fmt.Fprintf(stdout, "\nok: corrected p99 changed by %s, within the %g%% threshold\n", change, *threshold)
	}
	return 0
}

func loadSummary(path string) (report.Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return report.Summary{}, err
	}
	defer f.Close()
	s, err := report.ReadSummary(f)
	if err != nil {
		return report.Summary{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// p99Change returns the p99 row's change exactly as the table shows it.
func p99Change(c report.Comparison) string {
	for _, r := range c.Latency {
		if r.Label == "p99" {
			return r.Change
		}
	}
	return "n/a"
}

// windowText formats the latest window: what the server is doing right now,
// as opposed to the run-wide totals before it.
func windowText(w runner.Window) string {
	if w.Responses == 0 {
		return "window: no completions"
	}
	return fmt.Sprintf("window: %.0f rps, p50 %s p99 %s max %s",
		w.RPS, report.FormatDuration(w.P50), report.FormatDuration(w.P99), report.FormatDuration(w.Max))
}

// readBody returns the request body: the literal flag value, or the contents
// of a file when the value starts with '@'.
func readBody(v string) ([]byte, error) {
	if path, ok := strings.CutPrefix(v, "@"); ok {
		return os.ReadFile(path)
	}
	return []byte(v), nil
}

func writeJSONFile(path string, s report.Summary) error { return writeJSONTo(path, s.WriteJSON) }

// writeJSONTo creates path and has write fill it, reporting a failed close.
func writeJSONTo(path string, write func(io.Writer) error) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return write(f)
}

// headerFlag collects repeated -H "Key: Value" flags.
type headerFlag http.Header

func (h *headerFlag) String() string { return fmt.Sprint(http.Header(*h)) }

func (h *headerFlag) Set(v string) error {
	key, val, ok := strings.Cut(v, ":")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return fmt.Errorf(`invalid header %q, want "Key: Value"`, v)
	}
	if *h == nil {
		*h = headerFlag{}
	}
	http.Header(*h).Add(key, strings.TrimSpace(val))
	return nil
}
