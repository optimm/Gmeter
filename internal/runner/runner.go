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

const (
	defaultTimeout     = 30 * time.Second
	defaultSampleEvery = time.Second
)

// Config describes one load test.
type Config struct {
	URL    string
	Method string      // default GET
	Header http.Header // optional
	Body   []byte      // optional

	// The load profile is either Rate for Duration (a single constant stage),
	// or Stages (steps and ramps). Set one or the other.
	Rate     float64       // intended requests per second
	Duration time.Duration // how long requests keep being scheduled
	Stages   []pacer.Stage
	Arrivals pacer.Arrivals // how requests are spread within the rate: Even (the default) or Poisson
	Seed     uint64         // random seed for Poisson arrivals; the same seed gives the same arrival times

	Workers int           // max requests in flight (== max connections)
	Timeout time.Duration // per request; default 30s

	// Spin is how long before each request's due time the scheduler stops
	// sleeping and busy-waits instead. Zero uses a plain timer, which on some
	// systems (macOS especially) wakes milliseconds late, and that lateness
	// counts as request latency. The cost is up to Spin of one CPU core's time
	// per request, and never more than a quarter of the gap between requests,
	// so about a quarter of one core at most; see pacer.WaitUntil and CapSpin.
	Spin time.Duration

	// SampleEvery is the length of one timeline window (default 1s). The
	// timeline is always recorded; Progress, if set, is also called once per
	// window with live counters.
	SampleEvery time.Duration
	Progress    func(Snapshot)
}

// Snapshot is a lock-free view of a run in progress.
type Snapshot struct {
	Elapsed   time.Duration
	Scheduled uint64 // requests handed to workers so far
	Completed uint64 // got an HTTP response (any status)
	Failed    uint64 // transport-level failures (timeouts, refused, ...)
	Late      uint64 // requests the scheduler reached after their due time (see Result.Late)

	// Window covers only the time since the previous snapshot.
	Window Window
}

// Window is what happened between two consecutive samples. Latencies are
// corrected (measured from each request's intended send time) and Max is
// accurate to ~0.1%.
type Window struct {
	End       time.Duration // elapsed time at the end of this window
	Interval  time.Duration // length of this window
	Partial   bool          // the closing window of a run, shorter than SampleEvery
	Scheduled uint64        // requests dispatched to workers in this window
	Responses uint64        // latency samples that completed in this window
	Failed    uint64        // transport failures in this window
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

	// Generator health: is Gmeter itself keeping time?
	//
	// TimerLag is how far past its due time the scheduler woke up, for the
	// requests it actually had to sleep for. It isolates timer and OS
	// scheduling overshoot, plus GC pauses in this process.
	//
	// Late counts requests the scheduler reached more than a millisecond after
	// their due time, so it had no chance to sleep. Either every worker was
	// busy (the scheduler was blocked handing off the previous request: raise
	// -workers, or the server is slow) or the generator was starved of CPU.
	// Late requests are still measured from their due time.
	TimerLag *hist.Histogram
	Late     uint64

	// Timeline is one Window per SampleEvery, in time order, closed by a
	// Partial window for the tail. The windows add up exactly to the totals
	// above, so the per-second view and the summary can never disagree.
	Timeline []Window

	// Stages has one entry per stage of the profile, in order. Each total above
	// is the sum of the same figure over the stages.
	Stages []StageResult
}

// lateTolerance is how far past its due time the scheduler may reach a request
// before it counts as late. Without it, random arrivals would trip the count on
// a healthy generator: two requests a few microseconds apart leave the second
// one overdue by microseconds when the scheduler finishes the first. Real
// saturation is far above this: a worker stuck in a slow request leaves the
// scheduler blocked, and the requests behind it milliseconds or more late.
const lateTolerance = time.Millisecond

// StageResult is what happened to the requests that were due during one stage
// of the profile. Attribution is by due time: a request due in stage 1 that
// the server only answered during stage 2 still counts for stage 1, because
// it is the load that was being offered when it was due that is being tested.
type StageResult struct {
	Stage     pacer.Stage
	Scheduled uint64 // requests dispatched to workers
	Dropped   uint64 // owed by the stage but never dispatched
	Failed    uint64 // transport failures
	// Corrected and Service are as in Result, for this stage's requests only.
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
	sched, err := pacer.New(start, cfg.Stages, cfg.Arrivals, cfg.Seed)
	if err != nil {
		return nil, err
	}

	// If the server is hung the backlog would take forever to drain, so the
	// scheduler abandons undispatched requests once the run is overdue by one
	// request timeout. Workers keep using the parent ctx and finish normally.
	schedCtx, cancelSched := context.WithDeadline(ctx, start.Add(sched.Duration()+cfg.Timeout))
	defer cancelSched()

	var live counters
	tallies := make([]*tally, cfg.Workers)
	for i := range tallies {
		tallies[i] = newTally()
	}
	stopSampler := startSampler(cfg, &live, start, tallies)

	stages := make([]stageTally, len(cfg.Stages))
	scheduledIn, droppedIn := make([]uint64, len(cfg.Stages)), make([]uint64, len(cfg.Stages)) // only this goroutine writes them
	jobs := make(chan job, cfg.Workers)
	var wg sync.WaitGroup
	for _, t := range tallies {
		w := &worker{client: client, cfg: cfg, live: &live, t: t, stages: stages}
		wg.Go(func() { w.run(ctx, jobs) })
	}

	// Scheduler. Blocking on a full channel is deliberate: the schedule keeps
	// its place on the clock, so requests that had to wait are measured from
	// when they were due, not from when a worker became free.
	timerLag := hist.New() // written only by this goroutine
	var dropped uint64
	var prev time.Time // when the previous request was due
loop:
	for {
		at, stage, ok := sched.Next()
		if !ok {
			break
		}
		if now := time.Now(); now.Before(at) {
			var gap time.Duration // to the previous request; the first one is due at the start
			if !prev.IsZero() {
				gap = at.Sub(prev)
			}
			if pacer.WaitUntil(schedCtx, at, pacer.CapSpin(cfg.Spin, gap)) != nil {
				dropped++ // taken from the schedule but never dispatched
				droppedIn[stage]++
				break
			}
			timerLag.Record(time.Since(at))
		} else if now.Sub(at) > lateTolerance {
			live.late.Add(1) // behind schedule when we got here
		}
		prev = at
		select {
		case jobs <- job{at: at, stage: stage}:
			live.scheduled.Add(1)
			scheduledIn[stage]++
		case <-schedCtx.Done():
			dropped++
			droppedIn[stage]++
			break loop
		}
	}
	// Everything the schedule still owes from here on counts as dropped.
	for {
		_, stage, ok := sched.Next()
		if !ok {
			break
		}
		dropped++
		droppedIn[stage]++
	}

	close(jobs)
	wg.Wait()
	timeline := stopSampler() // after the workers: its closing window sees every sample

	res := &Result{
		Elapsed:   time.Since(start),
		Scheduled: live.scheduled.Load(),
		Dropped:   dropped,
		Statuses:  map[int]uint64{},
		Errors:    map[string]uint64{},
		Corrected: hist.New(),
		Service:   hist.New(),
		TimerLag:  timerLag,
		Late:      live.late.Load(),
		Timeline:  timeline,
	}
	for i := range stages {
		st := &stages[i] // the workers have exited: no lock needed
		r := StageResult{
			Stage:     cfg.Stages[i],
			Scheduled: scheduledIn[i],
			Dropped:   droppedIn[i],
			Failed:    st.failed,
			Corrected: st.corrected,
			Service:   st.service,
		}
		if r.Corrected == nil { // no response was ever recorded in this stage
			r.Corrected, r.Service = hist.New(), hist.New()
		}
		res.Stages = append(res.Stages, r)
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
	switch {
	case c.SampleEvery < 0:
		return c, fmt.Errorf("runner: sample interval must not be negative, got %v", c.SampleEvery)
	case c.SampleEvery == 0:
		c.SampleEvery = defaultSampleEvery
	}
	if c.Spin < 0 {
		return c, fmt.Errorf("runner: spin must not be negative, got %v", c.Spin)
	}
	switch {
	case len(c.Stages) > 0 && (c.Rate != 0 || c.Duration != 0):
		return c, fmt.Errorf("runner: set either Rate and Duration or Stages, not both")
	case len(c.Stages) == 0:
		c.Stages = pacer.ConstantRate(c.Rate, c.Duration)
	}
	if _, err := pacer.New(time.Time{}, c.Stages, c.Arrivals, c.Seed); err != nil {
		return c, err
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
	scheduled, completed, failed, late atomic.Uint64
}

func (c *counters) snapshot(start time.Time) Snapshot {
	return Snapshot{
		Elapsed:   time.Since(start),
		Scheduled: c.scheduled.Load(),
		Completed: c.completed.Load(),
		Failed:    c.failed.Load(),
		Late:      c.late.Load(),
	}
}

// startSampler launches the goroutine that builds the timeline (and calls
// cfg.Progress, if set) and returns a function that stops it, waits until its
// last callback has returned, and hands back the timeline.
//
// Each tick it takes a cumulative snapshot of every worker's corrected-latency
// Recorder and subtracts the previous tick's snapshot to get the window. It
// pulls rather than having workers push: during a stall the workers are
// blocked inside a request and could not push anything, which is exactly when
// the display must keep updating.
//
// stop must be called after the workers have exited. It then closes the run
// with one more, Partial, window covering the tail since the last tick, which
// is what makes the windows add up to the totals.
func startSampler(cfg Config, live *counters, start time.Time, tallies []*tally) (stop func() []Window) {
	done, exited := make(chan struct{}), make(chan struct{})
	var timeline []Window // owned by the goroutine until exited is closed
	go func() {
		defer close(exited)
		tick := time.NewTicker(cfg.SampleEvery)
		defer tick.Stop()

		prev, prevAt := hist.New(), start
		var prevScheduled, prevFailed uint64
		sample := func(now time.Time, partial bool) Window {
			cur := hist.New()
			for _, t := range tallies {
				t.corrected.AddTo(cur)
			}
			scheduled, failed := live.scheduled.Load(), live.failed.Load()
			w := windowOf(cur.Delta(prev), now.Sub(prevAt))
			w.End = now.Sub(start)
			w.Partial = partial
			w.Scheduled, w.Failed = scheduled-prevScheduled, failed-prevFailed
			prev, prevAt, prevScheduled, prevFailed = cur, now, scheduled, failed
			return w
		}

		for {
			select {
			case now := <-tick.C:
				w := sample(now, false)
				timeline = append(timeline, w)
				if cfg.Progress != nil {
					snap := live.snapshot(start)
					snap.Window = w
					cfg.Progress(snap)
				}
			case <-done:
				if w := sample(time.Now(), true); w.Scheduled+w.Responses+w.Failed > 0 {
					timeline = append(timeline, w)
				}
				return
			}
		}
	}()
	return func() []Window {
		close(done)
		<-exited
		return timeline
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

// job is one request handed from the scheduler to a worker.
type job struct {
	at    time.Time // when it was due: latency is measured from here
	stage int       // the stage of the profile it was due in
}

// stageTally is one stage's share of the results. Every worker records into
// it, so it has a lock; that is cheap next to an HTTP round trip, and it
// allocates its histograms only when a stage gets a first response.
type stageTally struct {
	mu                 sync.Mutex
	corrected, service *hist.Histogram
	failed             uint64
}

type worker struct {
	client *http.Client
	cfg    Config
	live   *counters
	t      *tally
	stages []stageTally
}

func (w *worker) run(ctx context.Context, jobs <-chan job) {
	for j := range jobs {
		w.do(ctx, j)
	}
}

func (w *worker) do(ctx context.Context, j job) {
	intended := j.at
	reqCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()

	var body io.Reader
	if len(w.cfg.Body) > 0 {
		body = bytes.NewReader(w.cfg.Body)
	}
	req, err := http.NewRequestWithContext(reqCtx, w.cfg.Method, w.cfg.URL, body)
	if err != nil { // validated up front; defensive only
		w.fail("other", j.stage)
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
		w.fail(kind, j.stage)
		if kind == "timeout" {
			w.record(j.stage, intended, sent, finished)
		}
		return
	}
	w.t.statuses[status]++
	w.live.completed.Add(1)
	w.record(j.stage, intended, sent, finished)
}

func (w *worker) record(stage int, intended, sent, finished time.Time) {
	corrected, service := finished.Sub(intended), finished.Sub(sent)
	w.t.corrected.Record(corrected)
	w.t.service.Record(service)

	st := &w.stages[stage]
	st.mu.Lock()
	if st.corrected == nil {
		st.corrected, st.service = hist.New(), hist.New()
	}
	st.corrected.Record(corrected)
	st.service.Record(service)
	st.mu.Unlock()
}

func (w *worker) fail(kind string, stage int) {
	w.t.errors[kind]++
	w.live.failed.Add(1)

	st := &w.stages[stage]
	st.mu.Lock()
	st.failed++
	st.mu.Unlock()
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
