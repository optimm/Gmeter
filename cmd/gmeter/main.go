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

	"github.com/optimm/Gmeter/internal/report"
	"github.com/optimm/Gmeter/internal/runner"
)

// sampleEvery is a variable so tests can shorten it.
var sampleEvery = time.Second

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

	fs := flag.NewFlagSet("gmeter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		rate     = fs.Float64("rate", 100, "target requests per second (open loop)")
		duration = fs.Duration("duration", 10*time.Second, "how long to keep scheduling requests")
		workers  = fs.Int("workers", 50, "max requests in flight, which is also the max number of connections")
		timeout  = fs.Duration("timeout", 10*time.Second, "per-request timeout")
		method   = fs.String("method", http.MethodGet, "HTTP method")
		body     = fs.String("body", "", "request body; prefix with @ to read it from a file")
		jsonOut  = fs.String("json", "", "also write a JSON summary to this path ('-' writes only the JSON, to stdout)")
		quiet    = fs.Bool("quiet", false, "no progress output on stderr")
		headers  headerFlag
	)
	fs.Var(&headers, "H", `extra request header "Key: Value" (repeatable)`)
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

	reqBody, err := readBody(*body)
	if err != nil {
		fmt.Fprintf(stderr, "gmeter: cannot read -body: %v\n", err)
		return 2
	}

	cfg := runner.Config{
		URL:         target,
		Method:      strings.ToUpper(*method),
		Header:      http.Header(headers),
		Body:        reqBody,
		Rate:        *rate,
		Duration:    *duration,
		Workers:     *workers,
		Timeout:     *timeout,
		SampleEvery: sampleEvery, // the same windows whether or not progress is shown
	}
	shownURL := redactURL(cfg.URL) // anything printed or saved uses this, never cfg.URL
	if !*quiet {
		fmt.Fprintf(stderr, "gmeter: %s %s at %g rps for %v, up to %d in flight\n",
			cfg.Method, shownURL, cfg.Rate, cfg.Duration, cfg.Workers)
		cfg.Progress = func(s runner.Snapshot) {
			fmt.Fprintf(stderr, "[%5.1fs] %d scheduled, %d completed, %d failed, %d late | %s\n",
				s.Elapsed.Seconds(), s.Scheduled, s.Completed, s.Failed, s.Late, windowText(s.Window))
		}
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

	summary := report.Summarize(res, cfg.Rate)
	summary.Run = report.RunInfo{
		URL:             shownURL,
		Method:          cfg.Method,
		Workers:         cfg.Workers,
		DurationSeconds: cfg.Duration.Seconds(),
		TimeoutSeconds:  cfg.Timeout.Seconds(),
		StartedAt:       started.Format(time.RFC3339),
	}
	if *jsonOut == "-" {
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
	if *jsonOut != "" {
		if err := writeJSONFile(*jsonOut, summary); err != nil {
			fmt.Fprintf(stderr, "gmeter: writing %s: %v\n", *jsonOut, err)
			return 1
		}
	}
	return 0
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

func writeJSONFile(path string, s report.Summary) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return s.WriteJSON(f)
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
