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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/optimm/Gmeter/internal/report"
	"github.com/optimm/Gmeter/internal/runner"
)

// progressEvery is a variable so tests can shorten it.
var progressEvery = time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process-global parts. It returns the exit code:
// 0 success, 2 bad usage or configuration, 1 failure while running.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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
	url := fs.Arg(0)

	reqBody, err := readBody(*body)
	if err != nil {
		fmt.Fprintf(stderr, "gmeter: cannot read -body: %v\n", err)
		return 2
	}

	cfg := runner.Config{
		URL:      url,
		Method:   strings.ToUpper(*method),
		Header:   http.Header(headers),
		Body:     reqBody,
		Rate:     *rate,
		Duration: *duration,
		Workers:  *workers,
		Timeout:  *timeout,
	}
	if !*quiet {
		fmt.Fprintf(stderr, "gmeter: %s %s at %g rps for %v, up to %d in flight\n",
			cfg.Method, cfg.URL, cfg.Rate, cfg.Duration, cfg.Workers)
		cfg.ProgressEvery = progressEvery
		cfg.Progress = func(s runner.Snapshot) {
			fmt.Fprintf(stderr, "[%5.1fs] %d scheduled, %d completed, %d failed\n",
				s.Elapsed.Seconds(), s.Scheduled, s.Completed, s.Failed)
		}
	}

	res, err := runner.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gmeter: %v\n", err)
		return 2
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "gmeter: interrupted; the report covers the partial run")
	}

	summary := report.Summarize(res, cfg.Rate)
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
