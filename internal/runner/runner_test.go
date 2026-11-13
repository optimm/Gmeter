package runner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
)

func baseConfig(url string) Config {
	return Config{
		URL:      url,
		Method:   "GET",
		Rate:     200,
		Duration: 500 * time.Millisecond,
		Workers:  8,
		Timeout:  2 * time.Second,
	}
}

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSchedulesExactlyRateTimesDuration(t *testing.T) {
	srv := okServer(t)

	res, err := Run(context.Background(), baseConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	// 200 req/s for 0.5s: request n is due at n*5ms, so n = 0..99.
	if res.Scheduled != 100 {
		t.Fatalf("Scheduled = %d, want 100", res.Scheduled)
	}
	if res.Statuses[200] != 100 {
		t.Fatalf("Statuses[200] = %d, want 100 (statuses: %v)", res.Statuses[200], res.Statuses)
	}
	if len(res.Errors) != 0 || res.Dropped != 0 {
		t.Fatalf("unexpected errors=%v dropped=%d", res.Errors, res.Dropped)
	}
	if res.Corrected.Count() != 100 || res.Service.Count() != 100 {
		t.Fatalf("histogram counts corrected=%d service=%d, want 100/100",
			res.Corrected.Count(), res.Service.Count())
	}
}

// The reason this tool exists. One connection, and the server freezes for
// 400ms on the very first request. A closed-loop tester would record a single
// slow sample and ~60 fast ones. An open-loop tester must also account for
// the ~40 requests that were due during the freeze and had to wait for it.
func TestCoordinatedOmissionIsCorrected(t *testing.T) {
	var first atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			time.Sleep(400 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := baseConfig(srv.URL)
	cfg.Rate = 100
	cfg.Duration = time.Second
	cfg.Workers = 1

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	service := res.Service.Quantile(0.9)
	corrected := res.Corrected.Quantile(0.9)
	if service >= 100*time.Millisecond {
		t.Errorf("service p90 = %v; only one request was slow so this should be small", service)
	}
	if corrected < 100*time.Millisecond {
		t.Errorf("corrected p90 = %v; requests queued behind the 400ms stall must show up as slow", corrected)
	}
	if res.Corrected.Max() < 350*time.Millisecond {
		t.Errorf("corrected max = %v, want >= ~400ms", res.Corrected.Max())
	}
}

func TestSendsConfiguredMethodHeadersAndBody(t *testing.T) {
	var mu sync.Mutex
	var gotMethod, gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotHeader, gotBody = r.Method, r.Header.Get("X-Test"), string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := baseConfig(srv.URL)
	cfg.Method = "POST"
	cfg.Header = http.Header{"X-Test": []string{"gmeter"}}
	cfg.Body = []byte(`{"hello":"world"}`)
	cfg.Rate, cfg.Duration = 20, 100*time.Millisecond

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotMethod != "POST" || gotHeader != "gmeter" || gotBody != `{"hello":"world"}` {
		t.Fatalf("server saw method=%q header=%q body=%q", gotMethod, gotHeader, gotBody)
	}
}

func TestHTTPErrorStatusesAreCountedNotTreatedAsTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	res, err := Run(context.Background(), baseConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Statuses[500] != res.Scheduled {
		t.Fatalf("Statuses[500] = %d, want %d", res.Statuses[500], res.Scheduled)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none for HTTP 500 responses", res.Errors)
	}
	if res.Corrected.Count() != res.Scheduled {
		t.Fatalf("a 500 is still a response with a latency: histogram has %d samples, want %d",
			res.Corrected.Count(), res.Scheduled)
	}
}

func TestConnectionRefusedIsClassifiedAndNotRecordedAsLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	res, err := Run(context.Background(), baseConfig(url))
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors["connection_refused"] != res.Scheduled {
		t.Fatalf("Errors = %v, want connection_refused=%d", res.Errors, res.Scheduled)
	}
	if res.Corrected.Count() != 0 {
		t.Fatalf("fast connection failures must not pollute latency: %d samples", res.Corrected.Count())
	}
}

func TestTimeoutsAreCountedAndKeepTheirLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the client gives up
	}))
	t.Cleanup(srv.Close)

	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 50, 200*time.Millisecond // 10 requests
	cfg.Workers = 10
	cfg.Timeout = 50 * time.Millisecond

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors["timeout"] != 10 {
		t.Fatalf("Errors = %v, want timeout=10", res.Errors)
	}
	// Dropping timeouts from the histogram would make a hung server look fast.
	if res.Corrected.Count() != 10 || res.Corrected.Min() < 50*time.Millisecond {
		t.Fatalf("timeouts should be recorded at >=50ms: count=%d min=%v",
			res.Corrected.Count(), res.Corrected.Min())
	}
}

func TestStopsPromptlyAndAccountsForUnsentRequestsOnCancel(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 100, 10*time.Second // 1000 owed

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)

	start := time.Now()
	res, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run took %v after cancel", elapsed)
	}
	if res.Dropped == 0 {
		t.Fatal("Dropped = 0, want the never-sent requests to be reported")
	}
	if res.Scheduled+res.Dropped != 1000 {
		t.Fatalf("Scheduled(%d)+Dropped(%d) = %d, want every owed request (1000) accounted for",
			res.Scheduled, res.Dropped, res.Scheduled+res.Dropped)
	}
}

func TestGivesUpOnBacklogAfterAGracePeriodWhenServerIsHung(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 100, 200*time.Millisecond // 20 owed
	cfg.Workers = 1                                    // one hung request at a time => backlog grows
	cfg.Timeout = 50 * time.Millisecond

	start := time.Now()
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Run took %v; it must not grind through the whole backlog at 50ms each", elapsed)
	}
	if res.Dropped == 0 || res.Scheduled+res.Dropped != 20 {
		t.Fatalf("Scheduled=%d Dropped=%d, want a non-zero Dropped and a sum of 20", res.Scheduled, res.Dropped)
	}
}

func TestReportsProgressWhileRunning(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 100, 350*time.Millisecond
	cfg.SampleEvery = 100 * time.Millisecond

	var mu sync.Mutex
	var snaps []Snapshot
	cfg.Progress = func(s Snapshot) {
		mu.Lock()
		snaps = append(snaps, s)
		mu.Unlock()
	}

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(snaps) < 2 {
		t.Fatalf("got %d progress snapshots, want at least 2", len(snaps))
	}
	last := snaps[len(snaps)-1]
	if last.Scheduled == 0 || last.Elapsed <= 0 {
		t.Fatalf("last snapshot looks empty: %+v", last)
	}
	for i := 1; i < len(snaps); i++ {
		if snaps[i].Scheduled < snaps[i-1].Scheduled {
			t.Fatalf("Scheduled went backwards: %+v then %+v", snaps[i-1], snaps[i])
		}
	}
}

func TestRejectsInvalidConfig(t *testing.T) {
	good := baseConfig("http://127.0.0.1:1")
	cases := map[string]func(*Config){
		"zero rate":        func(c *Config) { c.Rate = 0 },
		"zero duration":    func(c *Config) { c.Duration = 0 },
		"zero workers":     func(c *Config) { c.Workers = 0 },
		"empty url":        func(c *Config) { c.URL = "" },
		"non-http scheme":  func(c *Config) { c.URL = "ftp://example.com/x" },
		"negative timeout": func(c *Config) { c.Timeout = -time.Second },
		"negative spin":    func(c *Config) { c.Spin = -time.Microsecond },
		"stages and rate": func(c *Config) {
			c.Stages = pacer.ConstantRate(10, time.Second) // Rate and Duration are still set
		},
		"a stage with no duration": func(c *Config) {
			c.Rate, c.Duration = 0, 0
			c.Stages = []pacer.Stage{{Duration: 0, From: 10, To: 10}}
		},
		"stages that schedule nothing": func(c *Config) {
			c.Rate, c.Duration = 0, 0
			c.Stages = []pacer.Stage{{Duration: time.Second}}
		},
		"unknown arrivals": func(c *Config) { c.Arrivals = pacer.Arrivals(9) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := good
			mutate(&cfg)
			if _, err := Run(context.Background(), cfg); err == nil {
				t.Fatal("Run succeeded, want a validation error")
			}
		})
	}
}

func TestProgressCarriesPerWindowLatencyStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration, cfg.Workers = 100, 600*time.Millisecond, 20
	cfg.SampleEvery = 150 * time.Millisecond
	var mu sync.Mutex
	var snaps []Snapshot
	cfg.Progress = func(s Snapshot) {
		mu.Lock()
		snaps = append(snaps, s)
		mu.Unlock()
	}

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var windowed uint64
	var sawLatency bool
	for _, s := range snaps {
		w := s.Window
		windowed += w.Responses
		if w.Interval < 100*time.Millisecond || w.Interval > 400*time.Millisecond {
			t.Errorf("Window.Interval = %v, want ~150ms", w.Interval)
		}
		if w.Responses == 0 {
			continue
		}
		sawLatency = true
		if w.P50 < 20*time.Millisecond || w.P50 > 150*time.Millisecond {
			t.Errorf("Window.P50 = %v, want 20ms..150ms for a 20ms handler", w.P50)
		}
		if !(w.P50 <= w.P99 && w.P99 <= w.Max) {
			t.Errorf("window percentiles out of order: p50=%v p99=%v max=%v", w.P50, w.P99, w.Max)
		}
		if w.RPS <= 0 {
			t.Errorf("Window.RPS = %v with %d responses", w.RPS, w.Responses)
		}
	}
	if !sawLatency {
		t.Fatal("no snapshot carried any windowed latency")
	}
	// Windows are disjoint slices of the run, so they can never add up to more than the whole.
	if windowed > res.Corrected.Count() {
		t.Fatalf("windows count %d samples but the run only has %d", windowed, res.Corrected.Count())
	}
}

// A run-wide p99 cannot say *when* a stall happened. The windows must.
func TestWindowsPinpointWhenAStallHappened(t *testing.T) {
	begin := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if el := time.Since(begin); el >= 200*time.Millisecond && el < 400*time.Millisecond {
			time.Sleep(400*time.Millisecond - el) // freeze until t=400ms
		}
	}))
	t.Cleanup(srv.Close)

	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration, cfg.Workers = 200, 800*time.Millisecond, 100
	cfg.SampleEvery = 100 * time.Millisecond
	var mu sync.Mutex
	var windows []Window
	cfg.Progress = func(s Snapshot) {
		mu.Lock()
		windows = append(windows, s.Window)
		mu.Unlock()
	}

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(windows) < 5 {
		t.Fatalf("got %d windows, want at least 5", len(windows))
	}
	if first := windows[0].P99; first >= 80*time.Millisecond {
		t.Errorf("first window (before the freeze) P99 = %v, want small", first)
	}
	var worst time.Duration
	for _, w := range windows {
		worst = max(worst, w.P99)
	}
	if worst < 100*time.Millisecond {
		t.Errorf("worst window P99 = %v; the freeze must show up in some window", worst)
	}
}

func TestRecordsTimerLagForRequestsItHadToWaitFor(t *testing.T) {
	srv := okServer(t)

	res, err := Run(context.Background(), baseConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.TimerLag.Count() == 0 {
		t.Fatal("TimerLag has no samples; the scheduler sleeps before almost every request")
	}
	// An idle machine wakes a timer within microseconds. 20ms means the generator itself is struggling.
	if p := res.TimerLag.Quantile(0.5); p > 20*time.Millisecond {
		t.Fatalf("median timer lag = %v, want well under 20ms", p)
	}
}

// Without spinning, a timer wakes late by an amount the OS picks (about 2ms at
// the median on macOS for a 100 rps schedule), and all of it is charged to the
// request as latency. The scheduler must use the spin window to remove it.
func TestSpinMakesTheSchedulerWakeOnTime(t *testing.T) {
	if testing.Short() {
		t.Skip("measures this machine's timer precision, which a shared CI runner cannot promise; run without -short")
	}
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate = 100 // one every 10ms: long enough sleeps for a timer to overshoot
	cfg.Duration = time.Second
	cfg.Spin = 200 * time.Microsecond

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.TimerLag.Count() == 0 {
		t.Fatal("TimerLag has no samples")
	}
	if p := res.TimerLag.Quantile(0.5); p > 250*time.Microsecond {
		t.Fatalf("median timer lag with Spin = %v, want < 250µs", p)
	}
}

func TestFastServerWithEnoughWorkersHasFewLateDispatches(t *testing.T) {
	srv := okServer(t)

	res, err := Run(context.Background(), baseConfig(srv.URL)) // 100 requests, 8 workers
	if err != nil {
		t.Fatal(err)
	}
	if res.Late > 10 {
		t.Fatalf("Late = %d of %d; a fast server with idle workers should almost never leave the scheduler behind", res.Late, res.Scheduled)
	}
}

// When every worker is busy the scheduler blocks, so later requests are
// already overdue by the time it reaches them. That is saturation, and it has
// to be visible both live and in the result.
// Random arrivals put requests a few microseconds apart, so the scheduler often
// reaches one that is already due, by microseconds. That is not a generator in
// trouble: with an instant server and plenty of idle workers, the late count
// must stay low even at a rate where most gaps are shorter than a timer can
// resolve.
// The rule itself, on synthetic times, so it is checked on every machine: the
// end-to-end test below depends on how quiet the machine is.
func TestARequestIsLateOnlyWhenReachedMoreThanTheToleranceAfterItsDueTime(t *testing.T) {
	due := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before it is due", due.Add(-time.Second), false},
		{"exactly when due", due, false},
		{"microseconds late", due.Add(3 * time.Microsecond), false},
		{"exactly the tolerance late", due.Add(lateTolerance), false},
		{"a nanosecond past the tolerance", due.Add(lateTolerance + time.Nanosecond), true},
		{"seconds late", due.Add(2 * time.Second), true},
	} {
		if got := reachedLate(tc.now, due); got != tc.want {
			t.Errorf("%s: reachedLate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMicrosecondLatenessOfPoissonArrivalsIsNotCountedAsLate(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration, cfg.Workers = 5000, time.Second, 64
	cfg.Arrivals, cfg.Seed = pacer.Poisson, 3

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	frac := float64(res.Late) / float64(res.Scheduled)
	if frac <= 0.02 {
		return
	}
	// Too many were late. If this run's own timer lag shows the machine stalled
	// the scheduler for longer than the tolerance, they were late for real (that
	// is what the counter is for) and this run cannot tell that from the bug
	// below. Measured: an idle machine is 0.1% late with a timer lag p99.9 of about
	// 0.2ms, runs over 1.5% late all had a p99.9 above 2ms, a saturated machine is
	// 35-85% late, and the bug of counting every request that is already due is
	// about 16% late. A single long stall piles up overdue requests without moving
	// the p99.9, so the largest lag is checked as well.
	if lag := res.TimerLag.Quantile(0.999); lag > lateTolerance {
		t.Skipf("the machine was too busy to judge: %.1f%% late, but the scheduler's timer lag p99.9 was %v, over the %v tolerance", frac*100, lag, lateTolerance)
	}
	if lag := res.TimerLag.Max(); lag > 10*lateTolerance {
		t.Skipf("the machine was too busy to judge: %.1f%% late, but the scheduler once woke %v late", frac*100, lag)
	}
	t.Fatalf("Late = %d of %d (%.1f%%) on a quiet machine (timer lag p99.9 %v, max %v); requests reached microseconds late must not count",
		res.Late, res.Scheduled, frac*100, res.TimerLag.Quantile(0.999), res.TimerLag.Max())
}

func TestSaturatedWorkersShowUpAsLateDispatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration, cfg.Workers = 100, 300*time.Millisecond, 1 // 30 requests, 1 slow worker
	cfg.SampleEvery = 100 * time.Millisecond
	var mu sync.Mutex
	var lastLive uint64
	cfg.Progress = func(s Snapshot) {
		mu.Lock()
		lastLive = s.Late
		mu.Unlock()
	}

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Late < 15 {
		t.Fatalf("Late = %d of %d, want more than half to be late with one 30ms worker at 100 rps", res.Late, res.Scheduled)
	}
	mu.Lock()
	defer mu.Unlock()
	if lastLive == 0 {
		t.Fatal("Snapshot.Late stayed 0; saturation must be visible while the run is still going")
	}
}

func sum(ws []Window, f func(Window) uint64) (total uint64) {
	for _, w := range ws {
		total += f(w)
	}
	return total
}

// The timeline is recorded whether or not anyone asked for progress, and its
// windows must add up exactly to the whole run, so that nothing is lost between
// the per-second view and the totals.
func TestTimelineWindowsAddUpToTheWholeRun(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL) // 100 requests over 500ms
	cfg.SampleEvery = 100 * time.Millisecond
	// deliberately no Progress callback

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Timeline) < 4 {
		t.Fatalf("Timeline has %d windows, want at least 4 for a 500ms run sampled every 100ms", len(res.Timeline))
	}
	if got := sum(res.Timeline, func(w Window) uint64 { return w.Responses }); got != res.Corrected.Count() {
		t.Errorf("windows hold %d responses, the run has %d", got, res.Corrected.Count())
	}
	if got := sum(res.Timeline, func(w Window) uint64 { return w.Scheduled }); got != res.Scheduled {
		t.Errorf("windows hold %d scheduled requests, the run has %d", got, res.Scheduled)
	}
	for i := 1; i < len(res.Timeline); i++ {
		if res.Timeline[i].End <= res.Timeline[i-1].End {
			t.Fatalf("windows are not in time order: %v then %v", res.Timeline[i-1].End, res.Timeline[i].End)
		}
	}
}

func TestTimelineDefaultsToOneSecondWindowsAndKeepsTheTailAsAPartialWindow(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 100, 300*time.Millisecond // shorter than one sample interval

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Timeline) != 1 {
		t.Fatalf("Timeline has %d windows, want exactly 1 (the whole run is under the 1s default)", len(res.Timeline))
	}
	w := res.Timeline[0]
	if !w.Partial {
		t.Error("the closing window ends early, so it must be marked Partial")
	}
	if w.Responses != res.Corrected.Count() {
		t.Errorf("Responses = %d, want all %d", w.Responses, res.Corrected.Count())
	}
}

func TestTimelineCountsFailuresPerWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening: every request fails

	cfg := baseConfig(url)
	cfg.SampleEvery = 100 * time.Millisecond
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	var failedTotal uint64
	for _, n := range res.Errors {
		failedTotal += n
	}
	if got := sum(res.Timeline, func(w Window) uint64 { return w.Failed }); got != failedTotal || got == 0 {
		t.Fatalf("windows hold %d failures, the run has %d", got, failedTotal)
	}
}

func TestRejectsNegativeSampleInterval(t *testing.T) {
	cfg := baseConfig("http://127.0.0.1:1")
	cfg.SampleEvery = -time.Second
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("Run succeeded, want a validation error")
	}
}

// A step profile must actually change the rate. Window by window, the
// scheduler dispatches about 25 requests per 250ms at 100 rps, then about 75
// at 300 rps.
func TestAStepProfileChangesTheRateMidRun(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 0, 0
	cfg.Stages = []pacer.Stage{
		{Duration: 500 * time.Millisecond, From: 100, To: 100},
		{Duration: 500 * time.Millisecond, From: 300, To: 300},
	}
	cfg.SampleEvery = 250 * time.Millisecond

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 200 || res.Dropped != 0 { // 50 + 150
		t.Fatalf("scheduled %d, dropped %d, want 200 and 0", res.Scheduled, res.Dropped)
	}

	// Sum the windows that fall entirely in each stage; the sampler's ticks are
	// not aligned to the stage boundary to the millisecond, so allow slack.
	var first, second uint64
	for _, w := range res.Timeline {
		switch {
		case w.End <= 560*time.Millisecond:
			first += w.Scheduled
		case w.End > 700*time.Millisecond:
			second += w.Scheduled
		}
	}
	if first < 35 || first > 70 || second < 105 || second > 160 {
		t.Fatalf("dispatched %d in the first stage's windows and %d in the second's, want about 50 and about 150 (timeline %+v)", first, second, res.Timeline)
	}
}

func TestARampSchedulesTheAreaUnderIt(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 0, 0
	cfg.Stages = []pacer.Stage{{Duration: time.Second, From: 0, To: 200}} // 100 requests

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 100 {
		t.Fatalf("scheduled %d, want 100", res.Scheduled)
	}
}

// With the same seed Poisson arrivals are the same arrivals, so the run
// dispatches exactly what the schedule for that seed holds. (The count is a
// random number around 100: seed 5 is checked below not to be exactly 100,
// which is what evenly spaced arrivals would give.)
func TestPoissonArrivalsFollowTheSeed(t *testing.T) {
	const seed = 5
	stages := pacer.ConstantRate(100, time.Second)
	sched, err := pacer.New(time.Now(), stages, pacer.Poisson, seed)
	if err != nil {
		t.Fatal(err)
	}
	var want uint64
	for {
		if _, _, ok := sched.Next(); !ok {
			break
		}
		want++
	}
	if want == 100 {
		t.Fatal("this seed happens to give exactly 100 requests, so the test cannot tell Poisson from even spacing: pick another seed")
	}

	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 0, 0
	cfg.Stages = stages
	cfg.Arrivals, cfg.Seed = pacer.Poisson, seed
	for range 2 {
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if res.Scheduled != want {
			t.Fatalf("scheduled %d, want %d (what the Poisson schedule for seed %d holds)", res.Scheduled, want, seed)
		}
	}
}

// latencyFrom answers instantly until `from` after the test started, and then
// takes `slow` per request.
func latencyFrom(t *testing.T, from, slow time.Duration) *httptest.Server {
	t.Helper()
	begin := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Since(begin) >= from {
			time.Sleep(slow)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stagedConfig(url string, stages ...pacer.Stage) Config {
	cfg := baseConfig(url)
	cfg.Rate, cfg.Duration = 0, 0
	cfg.Stages = stages
	return cfg
}

// A step test is only useful if it says which step the server stopped coping
// at. Here the server turns slow halfway through, exactly at the step.
func TestResultsAreBrokenDownByStage(t *testing.T) {
	srv := latencyFrom(t, 500*time.Millisecond, 20*time.Millisecond)
	cfg := stagedConfig(srv.URL,
		pacer.Stage{Duration: 500 * time.Millisecond, From: 100, To: 100},
		pacer.Stage{Duration: 500 * time.Millisecond, From: 300, To: 300})
	cfg.Workers = 64 // enough that the slow stage does not saturate the workers

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stages) != 2 {
		t.Fatalf("%d stage results, want 2", len(res.Stages))
	}
	if res.Stages[0].Stage != cfg.Stages[0] || res.Stages[1].Stage != cfg.Stages[1] {
		t.Errorf("stage results carry %v and %v, want the configured stages", res.Stages[0].Stage, res.Stages[1].Stage)
	}
	if res.Stages[0].Scheduled != 50 || res.Stages[1].Scheduled != 150 {
		t.Errorf("scheduled per stage = %d and %d, want 50 and 150", res.Stages[0].Scheduled, res.Stages[1].Scheduled)
	}
	if p50 := res.Stages[0].Corrected.Quantile(0.5); p50 > 10*time.Millisecond {
		t.Errorf("stage 1 (fast server) p50 = %v, want a few milliseconds at most", p50)
	}
	if p50 := res.Stages[1].Corrected.Quantile(0.5); p50 < 20*time.Millisecond {
		t.Errorf("stage 2 (20ms per request) p50 = %v, want at least 20ms", p50)
	}
	if svc := res.Stages[1].Service.Quantile(0.5); svc < 20*time.Millisecond {
		t.Errorf("stage 2 service p50 = %v, want at least 20ms", svc)
	}

	// The stages partition the run: nothing is lost or counted twice.
	var scheduled, responses uint64
	for _, st := range res.Stages {
		scheduled += st.Scheduled
		responses += st.Corrected.Count()
	}
	if scheduled != res.Scheduled || responses != res.Corrected.Count() {
		t.Errorf("stages add up to %d scheduled and %d responses, the run has %d and %d",
			scheduled, responses, res.Scheduled, res.Corrected.Count())
	}
}

func TestFailuresAreCountedInTheStageTheyWereDueIn(t *testing.T) {
	cfg := stagedConfig("http://127.0.0.1:1", // nothing listens: every request is refused
		pacer.Stage{Duration: 200 * time.Millisecond, From: 100, To: 100},
		pacer.Stage{Duration: 200 * time.Millisecond, From: 200, To: 200})

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stages[0].Failed != 20 || res.Stages[1].Failed != 40 {
		t.Fatalf("failed per stage = %d and %d, want 20 and 40", res.Stages[0].Failed, res.Stages[1].Failed)
	}
	if res.Stages[0].Corrected.Count() != 0 || res.Stages[1].Corrected.Count() != 0 {
		t.Errorf("refused connections must not be recorded as latency")
	}
}

func TestCancelledRunAccountsForEveryRequestOfEveryStage(t *testing.T) {
	srv := okServer(t)
	cfg := stagedConfig(srv.URL,
		pacer.Stage{Duration: time.Second, From: 100, To: 100},
		pacer.Stage{Duration: time.Second, From: 200, To: 200})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	res, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Each request was either dispatched or dropped, in the stage it was due.
	for i, want := range []uint64{100, 200} {
		if got := res.Stages[i].Scheduled + res.Stages[i].Dropped; got != want {
			t.Errorf("stage %d: scheduled %d + dropped %d = %d, want %d", i+1, res.Stages[i].Scheduled, res.Stages[i].Dropped, got, want)
		}
	}
	if res.Stages[1].Scheduled != 0 {
		t.Errorf("stage 2 never started but dispatched %d", res.Stages[1].Scheduled)
	}
	if res.Stages[0].Dropped+res.Stages[1].Dropped != res.Dropped {
		t.Errorf("stage drops %d + %d != run drops %d", res.Stages[0].Dropped, res.Stages[1].Dropped, res.Dropped)
	}
}

// A server that answers every request with a 500 is failing, however fast it
// is, so the stage results count bad statuses (outside 200-399) separately from
// transport failures, in the stage the request was due in.
func TestBadStatusesAreCountedInTheStageTheyWereDueIn(t *testing.T) {
	begin := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Since(begin) >= 300*time.Millisecond { // the second stage only
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	cfg := stagedConfig(srv.URL,
		pacer.Stage{Duration: 300 * time.Millisecond, From: 100, To: 100},
		pacer.Stage{Duration: 300 * time.Millisecond, From: 100, To: 100})

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The server flips at 300ms after it was created, a moment before the run's
	// second stage starts, so allow a request or two of slop at the boundary.
	if res.Stages[0].BadStatus > 2 {
		t.Errorf("stage 1 counted %d bad statuses, want about 0", res.Stages[0].BadStatus)
	}
	if res.Stages[1].BadStatus < 28 {
		t.Errorf("stage 2 counted %d bad statuses, want about 30", res.Stages[1].BadStatus)
	}
	if res.Stages[1].Failed != 0 {
		t.Errorf("a 500 is a response, not a transport failure, but stage 2 counted %d failures", res.Stages[1].Failed)
	}
}
