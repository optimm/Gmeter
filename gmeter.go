// Package gmeter is an open-loop HTTP load generator you can call from Go code:
// from a test, a benchmark or a deploy check, with the same engine as the
// gmeter command.
//
// Open loop means each request is sent when the schedule says, whether or not the
// earlier ones have been answered, and its latency is measured from that due time.
// When the server stalls, the requests that should have been sent during the stall
// show up as slow instead of silently not existing. A [Result] therefore has two
// latencies: [Result.Corrected], from when a request was due, and
// [Result.Service], from when it was sent. The gap between them is time requests
// spent waiting inside the generator, which a closed-loop tool never shows.
//
// Run a load and check a target:
//
//	res, err := gmeter.Run(ctx, gmeter.Config{URL: srv.URL, Rate: 500, Duration: 10 * time.Second})
//	if err != nil {
//		t.Fatal(err)
//	}
//	if err := res.Meets(gmeter.SLO{Quantile: 0.99, Latency: 50 * time.Millisecond}); err != nil {
//		t.Error(err) // says which part of the target was missed
//	}
//
// Or ask how much the server can take: [FindCapacity].
package gmeter

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/find"
	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/report"
	"github.com/optimm/Gmeter/internal/runner"
)

// Stage is one segment of a load profile. The rate moves in a straight line from
// From to To (requests per second) over Duration, so From == To is a constant
// rate, a larger To is a ramp up, and zero throughout is a pause.
type Stage struct {
	Duration time.Duration
	From, To float64
}

// Request is one thing to send. A [Config] with several sends them in turn, over
// and over, each at its place in the schedule.
type Request struct {
	Method string      // empty uses Config.Method
	Path   string      // path and query, starting with "/", appended to Config.URL
	Header http.Header // added to Config.Header; a name given here replaces the global value
	Body   []byte      // empty uses Config.Body
	// Label groups requests in the Sample hook, usually by endpoint.
	Label string
}

// Sample is one request as it happened, as given to [Config.OnRequest]. The times
// are offsets from the start of the run.
type Sample struct {
	Seq      uint64        // its index in the schedule, from 0
	Label    string        // the Request.Label it carried, if any
	Due      time.Duration // when the schedule said to send it: Corrected latency runs from here
	Sent     time.Duration // when it was handed to the client; later than Due if it had to wait for a worker
	Finished time.Duration // when its response was read, or it failed
	Status   int           // the HTTP status, or 0 if it got none
	Failure  string        // the kind of failure, such as "timeout" or "connection_refused"; empty if it got an answer
}

// Config says what to send and how fast. Everything but URL and the load has a
// default.
type Config struct {
	URL    string      // http://host:port or https://..., with an optional path prefix
	Method string      // default GET
	Header http.Header // sent with every request
	Body   []byte      // sent with every request

	// The load is either Rate for Duration, or Stages, not both.
	Rate     float64       // requests per second
	Duration time.Duration // how long requests keep being scheduled
	Stages   []Stage       // steps and ramps

	// Poisson spreads requests at random, with the same average rate, instead of
	// exactly evenly: real traffic is bursty and bursts queue. Seed repeats the
	// same arrival times; zero picks one.
	Poisson bool
	Seed    uint64

	// Requests, if set, is what to send instead of one request with Method, Header
	// and Body: the i-th request sent is Requests[i mod len(Requests)].
	Requests []Request

	Workers int           // most requests in flight, which is also the most connections; default 50
	Timeout time.Duration // per request; default 10s

	// TLS is the client configuration for https: the roots to trust, a client
	// certificate for mutual TLS, or InsecureSkipVerify. Nil verifies against the
	// system's roots.
	TLS *tls.Config

	// HTTP2 sends over HTTP/2 only, negotiated for https and h2c for http. A server
	// without it fails the requests instead of being measured over HTTP/1.1.
	HTTP2 bool

	// Spin is how long before each request's due time the scheduler busy-waits
	// instead of sleeping, for precise timing, at the cost of up to a quarter of a
	// core. Zero is 200µs; negative uses plain timers.
	Spin time.Duration

	// OnRequest, if set, is called once for each request that was sent, as soon as
	// it ends. It is called from the workers at the same time, so it must be safe
	// for concurrent use, and it must return quickly: a slow one delays the next
	// request and shows up as latency.
	OnRequest func(Sample)
}

const (
	defaultWorkers = 50
	defaultTimeout = 10 * time.Second
	defaultSpin    = 200 * time.Microsecond
)

// runnerConfig converts c, applying the defaults. withLoad says whether a load is
// required: a capacity search chooses its own.
func (c Config) runnerConfig(withLoad bool) (runner.Config, error) {
	switch {
	case c.URL == "":
		return runner.Config{}, errors.New("gmeter: Config.URL is required")
	case c.Workers < 0:
		return runner.Config{}, fmt.Errorf("gmeter: Workers must not be negative, got %d", c.Workers)
	case withLoad && len(c.Stages) == 0 && (c.Rate <= 0 || c.Duration <= 0):
		return runner.Config{}, fmt.Errorf("gmeter: set Rate and Duration, or Stages, to say what load to send (got Rate %v, Duration %v)", c.Rate, c.Duration)
	case !withLoad && (c.Rate != 0 || c.Duration != 0 || c.Stages != nil):
		return runner.Config{}, errors.New("gmeter: a capacity search chooses its own rates: leave Rate, Duration and Stages empty")
	}
	rc := runner.Config{
		URL:         c.URL,
		Method:      c.Method,
		Header:      c.Header,
		Body:        c.Body,
		Rate:        c.Rate,
		Duration:    c.Duration,
		Seed:        c.Seed,
		Workers:     cmpOr(c.Workers, defaultWorkers),
		Timeout:     cmpOr(c.Timeout, defaultTimeout),
		Spin:        c.Spin,
		TLS:         c.TLS,
		HTTP2:       c.HTTP2,
		Phases:      true,
		SampleEvery: time.Second,
	}
	switch {
	case c.Spin == 0:
		rc.Spin = defaultSpin
	case c.Spin < 0:
		rc.Spin = 0
	}
	if c.Poisson {
		rc.Arrivals = pacer.Poisson
	}
	for _, s := range c.Stages {
		rc.Stages = append(rc.Stages, pacer.Stage{Duration: s.Duration, From: s.From, To: s.To})
	}
	for _, r := range c.Requests {
		rc.Requests = append(rc.Requests, runner.Request{Method: r.Method, Target: r.Path, Header: r.Header, Body: r.Body, Label: r.Label})
	}
	if c.OnRequest != nil {
		rc.Sink = func(s runner.Sample) {
			c.OnRequest(Sample{Seq: s.Seq, Label: s.Label, Due: s.Due, Sent: s.Sent, Finished: s.Finished, Status: s.Status, Failure: s.Failure})
		}
	}
	return rc, nil
}

func cmpOr[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

// explain gives an internal error the package's name.
func explain(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, prefix := range []string{"runner: ", "find: "} {
		if rest, ok := strings.CutPrefix(msg, prefix); ok {
			return errors.New("gmeter: " + rest)
		}
	}
	return err
}

// Run sends the load Config describes and returns what happened. If ctx is
// cancelled the run stops early and the result covers what ran, with a nil error.
// An error means the Config could not run at all.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	rc, err := cfg.runnerConfig(true)
	if err != nil {
		return nil, err
	}
	res, err := runner.Run(ctx, rc)
	if err != nil {
		return nil, explain(err)
	}
	stages := rc.Stages
	if stages == nil {
		stages = pacer.ConstantRate(rc.Rate, rc.Duration)
	}
	duration, expected := pacer.Totals(stages)
	r := &Result{raw: res}
	if duration > 0 {
		r.targetRPS = expected / duration.Seconds()
	}
	return r, nil
}

// Result is what a run did. All of its latencies are in time.Duration.
type Result struct {
	raw       *runner.Result
	targetRPS float64
}

// Scheduled is how many requests the run sent.
func (r *Result) Scheduled() uint64 { return r.raw.Scheduled }

// Dropped is how many the schedule owed but never sent: the run was cancelled, or
// the server hung so long the backlog was abandoned.
func (r *Result) Dropped() uint64 { return r.raw.Dropped }

// Elapsed is how long the run took, to the last request's end.
func (r *Result) Elapsed() time.Duration { return r.raw.Elapsed }

// Statuses counts the HTTP responses by status code.
func (r *Result) Statuses() map[int]uint64 {
	out := make(map[int]uint64, len(r.raw.Statuses))
	for k, v := range r.raw.Statuses {
		out[k] = v
	}
	return out
}

// Errors counts the requests that got no response by the kind of failure:
// "timeout", "connection_refused", "connection_reset", "dns", "tls", "protocol" or
// "other".
func (r *Result) Errors() map[string]uint64 {
	out := make(map[string]uint64, len(r.raw.Errors))
	for k, v := range r.raw.Errors {
		out[k] = v
	}
	return out
}

// Responses is how many requests got a response, of any status.
func (r *Result) Responses() uint64 {
	var n uint64
	for _, c := range r.raw.Statuses {
		n += c
	}
	return n
}

// OK is how many responses had a status from 200 to 399.
func (r *Result) OK() uint64 {
	var n uint64
	for code, c := range r.raw.Statuses {
		if code >= 200 && code < 400 {
			n += c
		}
	}
	return n
}

// BadStatus is how many responses had a status outside 200 to 399.
func (r *Result) BadStatus() uint64 { return r.Responses() - r.OK() }

// Failed is how many requests got no response at all.
func (r *Result) Failed() uint64 {
	var n uint64
	for _, c := range r.raw.Errors {
		n += c
	}
	return n
}

// ErrorRate is the fraction of the requests sent that failed or got a bad status.
func (r *Result) ErrorRate() float64 {
	if r.raw.Scheduled == 0 {
		return 0
	}
	return float64(r.Failed()+r.BadStatus()) / float64(r.raw.Scheduled)
}

// Corrected is the latency at quantile q (0.99 for p99), measured from when each
// request was due. It counts responses of any status and timeouts, but not other
// failures: a refused connection says nothing about how fast the server answers.
func (r *Result) Corrected(q float64) time.Duration { return r.raw.Corrected.Quantile(q) }

// Service is the latency at quantile q measured from when each request was sent.
// It is never more than Corrected; the difference is time spent waiting for a
// free worker, which is the generator's, or the server's if it was too slow for
// the workers to keep up.
func (r *Result) Service(q float64) time.Duration { return r.raw.Service.Quantile(q) }

// WriteText writes the report the gmeter command prints.
func (r *Result) WriteText(w io.Writer) error { return r.summary().WriteText(w) }

// WriteJSON writes the summary the gmeter command saves with -json, which
// `gmeter diff` compares.
func (r *Result) WriteJSON(w io.Writer) error { return r.summary().WriteJSON(w) }

func (r *Result) summary() report.Summary { return report.Summarize(r.raw, r.targetRPS) }

// SLO is a target: the latency at one quantile, and how many errors are allowed.
// Latency is Corrected, measured from when each request was due.
type SLO struct {
	Quantile     float64       // 0.99 for p99; between 0 and 1, exclusive
	Latency      time.Duration // the limit at that quantile; positive
	MaxErrorRate float64       // failed requests plus bad statuses, as a fraction of those sent; 0 allows none
}

// ParseSLO reads a target as the command line writes it, such as "p99=50ms" or
// "p99.9=200ms". It allows no errors; set MaxErrorRate to allow some.
func ParseSLO(s string) (SLO, error) {
	slo, err := find.ParseSLO(s)
	if err != nil {
		return SLO{}, explain(err)
	}
	return SLO{Quantile: slo.Quantile, Latency: slo.Latency}, nil
}

func (s SLO) internal() (find.SLO, error) {
	switch {
	case s.Quantile <= 0 || s.Quantile >= 1:
		return find.SLO{}, fmt.Errorf("gmeter: SLO.Quantile must be between 0 and 1, such as 0.99 for p99, got %v", s.Quantile)
	case s.Latency <= 0:
		return find.SLO{}, fmt.Errorf("gmeter: SLO.Latency must be positive, got %v", s.Latency)
	case s.MaxErrorRate < 0 || s.MaxErrorRate > 1:
		return find.SLO{}, fmt.Errorf("gmeter: SLO.MaxErrorRate must be a fraction from 0 to 1, got %v", s.MaxErrorRate)
	}
	return find.SLO{Quantile: s.Quantile, Latency: s.Latency, MaxErrorRate: s.MaxErrorRate}, nil
}

// ErrTargetMissed is what an error from [Result.Meets] wraps when the run missed
// the target, as opposed to the target itself being invalid.
var ErrTargetMissed = errors.New("target missed")

// Meets reports why the run missed the target, or nil if it met it. It misses it
// when the latency at the target's quantile is over the limit, when more requests
// failed than the error budget allows, when requests were left unsent, or when
// the load generator could not keep time, in which case a good-looking result
// would be the generator's and not the server's. The error wraps
// [ErrTargetMissed], and its text says which.
func (r *Result) Meets(slo SLO) error {
	target, err := slo.internal()
	if err != nil {
		return err
	}
	st := runner.StageResult{
		Scheduled: r.raw.Scheduled,
		Dropped:   r.raw.Dropped,
		Failed:    r.Failed(),
		BadStatus: r.BadStatus(),
		Corrected: r.raw.Corrected,
		Service:   r.raw.Service,
	}
	if st.Corrected == nil { // a Result always has them; defensive only
		st.Corrected, st.Service = hist.New(), hist.New()
	}
	verdict, obs := find.Evaluate(target, 0, st, r.raw)
	if verdict == find.Pass {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrTargetMissed, strings.Join(obs.Reasons, "; "))
}
