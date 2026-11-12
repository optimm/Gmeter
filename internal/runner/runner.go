// Package runner drives an open-loop HTTP load test.
//
// Shape of a run:
//
//	scheduler ──(chan time.Time of intended send times)──▶ N workers
//	   │                                                      │
//	   │ fires on the clock, never waits for responses        │ one request at a time,
//	   ▼                                                      ▼ own histograms, no locks
//	 pacer.Schedule                                      tally ──merged──▶ Result
//
// Every request carries its *intended* send time. Latency is recorded twice:
// "corrected" (from the intended time, includes any time spent waiting for a
// free worker) and "service" (from the moment the request was actually sent).
// The gap between the two is exactly what a closed-loop tester hides
// (coordinated omission).
package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/pacer"
)

const defaultTimeout = 30 * time.Second

// Config describes one load test.
type Config struct {
	URL    string
	Method string      // default GET
	Header http.Header // optional
	Body   []byte      // optional

	Rate     float64       // intended requests per second
	Duration time.Duration // how long requests keep being scheduled
	Workers  int           // max requests in flight (== max connections)
	Timeout  time.Duration // per request; default 30s

	// Progress, if set, is called every ProgressEvery with live counters.
	Progress      func(Snapshot)
	ProgressEvery time.Duration
}

// Snapshot is a lock-free view of a run in progress.
type Snapshot struct {
	Elapsed   time.Duration
	Scheduled uint64 // requests handed to workers so far
	Completed uint64 // got an HTTP response (any status)
	Failed    uint64 // transport-level failures (timeouts, refused, ...)

	// Window covers only the time since the previous snapshot.
	Window Window
}

// Window is what happened between two consecutive snapshots. Latencies are
// corrected (measured from each request's intended send time) and Max is
// accurate to ~0.1%.
type Window struct {
	Interval  time.Duration // time since the previous snapshot
	Responses uint64        // latency samples that completed in this window
	RPS       float64       // Responses per second of Interval
	P50, P99  time.Duration
	Max       time.Duration
}

// Result is the outcome of a run.
type Result struct {
	Elapsed   time.Duration  // wall time until the last request finished
	Scheduled uint64         // requests dispatched to workers
	Dropped   uint64         // owed by the schedule but never dispatched (cancel / hung server)
	Statuses  map[int]uint64 // HTTP status code -> count
	Errors    map[string]uint64

	// Corrected is latency measured from each request's intended send time.
	// Service is latency measured from when it was actually sent.
	// Both include responses of any status and timeouts, but not other
	// transport failures: a connection refused in 50µs says nothing about
	// how fast the server answers.
	Corrected *hist.Histogram
	Service   *hist.Histogram
}

// Run executes the load test and blocks until it is finished or ctx is done.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}

	client := newClient(cfg.Workers)
	defer client.CloseIdleConnections()

	start := time.Now()
	sched, err := pacer.New(start, cfg.Rate)
	if err != nil {
		return nil, err
	}

	// If the server is hung the backlog would take forever to drain, so the
	// scheduler abandons undispatched requests once the run is overdue by one
	// request timeout. Workers keep using the parent ctx and finish normally.
	schedCtx, cancelSched := context.WithDeadline(ctx, start.Add(cfg.Duration+cfg.Timeout))
	defer cancelSched()

	var live counters
	tallies := make([]*tally, cfg.Workers)
	for i := range tallies {
		tallies[i] = newTally()
	}
	stopProgress := startProgress(cfg, &live, start, tallies)

	jobs := make(chan time.Time, cfg.Workers)
	var wg sync.WaitGroup
	for _, t := range tallies {
		w := &worker{client: client, cfg: cfg, live: &live, t: t}
		wg.Go(func() { w.run(ctx, jobs) })
	}

	// Scheduler. Blocking on a full channel is deliberate: the schedule keeps
	// its place on the clock, so requests that had to wait are measured from
	// when they were due, not from when a worker became free.
	var n uint64
loop:
	for ; ; n++ {
		at := sched.At(n)
		if at.Sub(start) >= cfg.Duration {
			break
		}
		if pacer.SleepUntil(schedCtx, at) != nil {
			break
		}
		select {
		case jobs <- at:
			live.scheduled.Add(1)
		case <-schedCtx.Done():
			break loop
		}
	}
	// n is the first request that was never dispatched; everything the
	// schedule still owed from here on counts as dropped.
	var dropped uint64
	for ; sched.At(n).Sub(start) < cfg.Duration; n++ {
		dropped++
	}

	close(jobs)
	wg.Wait()
	stopProgress()

	res := &Result{
		Elapsed:   time.Since(start),
		Scheduled: live.scheduled.Load(),
		Dropped:   dropped,
		Statuses:  map[int]uint64{},
		Errors:    map[string]uint64{},
		Corrected: hist.New(),
		Service:   hist.New(),
	}
	for _, t := range tallies {
		t.corrected.AddTo(res.Corrected)
		t.service.AddTo(res.Service)
		for code, c := range t.statuses {
			res.Statuses[code] += c
		}
		for kind, c := range t.errors {
			res.Errors[kind] += c
		}
	}
	return res, nil
}

func (c Config) normalize() (Config, error) {
	if c.Method == "" {
		c.Method = http.MethodGet
	}
	switch {
	case c.Timeout < 0:
		return c, fmt.Errorf("runner: timeout must not be negative, got %v", c.Timeout)
	case c.Timeout == 0:
		c.Timeout = defaultTimeout
	}
	if _, err := pacer.New(time.Time{}, c.Rate); err != nil {
		return c, err
	}
	if c.Duration <= 0 {
		return c, fmt.Errorf("runner: duration must be positive, got %v", c.Duration)
	}
	if c.Workers < 1 {
		return c, fmt.Errorf("runner: workers must be at least 1, got %d", c.Workers)
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return c, fmt.Errorf("runner: invalid url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("runner: url must be http(s)://host[:port]/..., got %q", c.URL)
	}
	if _, err := http.NewRequest(c.Method, c.URL, nil); err != nil {
		return c, fmt.Errorf("runner: invalid request: %w", err)
	}
	return c, nil
}

// newClient returns an HTTP/1.1 client capped at conns connections that
// neither follows redirects (a 302 is a result, not a reason to send another
// request) nor honours proxy environment variables.
func newClient(conns int) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        conns,
			MaxIdleConnsPerHost: conns,
			MaxConnsPerHost:     conns,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
			// A non-nil empty map disables HTTP/2: v0 is HTTP/1.1 only.
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// counters are the only state shared between goroutines while a run is live.
type counters struct {
	scheduled, completed, failed atomic.Uint64
}

func (c *counters) snapshot(start time.Time) Snapshot {
	return Snapshot{
		Elapsed:   time.Since(start),
		Scheduled: c.scheduled.Load(),
		Completed: c.completed.Load(),
		Failed:    c.failed.Load(),
	}
}

// startProgress launches the progress reporter, if configured, and returns a
// function that stops it and waits until its last callback has returned.
//
// Each tick it takes a cumulative snapshot of every worker's corrected-latency
// Recorder and subtracts the previous tick's snapshot to get the window. It
// pulls rather than having workers push: during a stall the workers are
// blocked inside a request and could not push anything, which is exactly when
// the display must keep updating.
func startProgress(cfg Config, live *counters, start time.Time, tallies []*tally) (stop func()) {
	if cfg.Progress == nil || cfg.ProgressEvery <= 0 {
		return func() {}
	}
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		tick := time.NewTicker(cfg.ProgressEvery)
		defer tick.Stop()
		prev, prevAt := hist.New(), start
		for {
			select {
			case now := <-tick.C:
				cur := hist.New()
				for _, t := range tallies {
					t.corrected.AddTo(cur)
				}
				snap := live.snapshot(start)
				snap.Window = windowOf(cur.Delta(prev), now.Sub(prevAt))
				prev, prevAt = cur, now
				cfg.Progress(snap)
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

func windowOf(d *hist.Histogram, interval time.Duration) Window {
	w := Window{
		Interval:  interval,
		Responses: d.Count(),
		P50:       d.Quantile(0.50),
		P99:       d.Quantile(0.99),
		Max:       d.Max(),
	}
	if interval > 0 {
		w.RPS = float64(d.Count()) / interval.Seconds()
	}
	return w
}

// tally is owned by exactly one worker while the run is live. Its histograms
// are Recorders so the progress goroutine can read them mid-run; its maps are
// plain because only Run touches them, after the workers have exited.
type tally struct {
	corrected, service *hist.Recorder
	statuses           map[int]uint64
	errors             map[string]uint64
}

func newTally() *tally {
	return &tally{
		corrected: hist.NewRecorder(),
		service:   hist.NewRecorder(),
		statuses:  map[int]uint64{},
		errors:    map[string]uint64{},
	}
}

type worker struct {
	client *http.Client
	cfg    Config
	live   *counters
	t      *tally
}

func (w *worker) run(ctx context.Context, jobs <-chan time.Time) {
	for intended := range jobs {
		w.do(ctx, intended)
	}
}

func (w *worker) do(ctx context.Context, intended time.Time) {
	reqCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()

	var body io.Reader
	if len(w.cfg.Body) > 0 {
		body = bytes.NewReader(w.cfg.Body)
	}
	req, err := http.NewRequestWithContext(reqCtx, w.cfg.Method, w.cfg.URL, body)
	if err != nil { // validated up front; defensive only
		w.fail("other")
		return
	}
	req.Header = w.cfg.Header.Clone()

	sent := time.Now()
	resp, err := w.client.Do(req)
	var status int
	if err == nil {
		status = resp.StatusCode
		// Drain so the connection goes back to the pool for reuse.
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	finished := time.Now()

	if err != nil {
		kind := classify(err)
		if ctx.Err() != nil { // the whole run was cancelled, not this request timing out
			kind = "canceled"
		}
		w.fail(kind)
		if kind == "timeout" {
			w.record(intended, sent, finished)
		}
		return
	}
	w.t.statuses[status]++
	w.live.completed.Add(1)
	w.record(intended, sent, finished)
}

func (w *worker) record(intended, sent, finished time.Time) {
	w.t.corrected.Record(finished.Sub(intended))
	w.t.service.Record(finished.Sub(sent))
}

func (w *worker) fail(kind string) {
	w.t.errors[kind]++
	w.live.failed.Add(1)
}

// classify buckets a transport error into a short, stable label.
func classify(err error) string {
	var dns *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "other"
	}
}
