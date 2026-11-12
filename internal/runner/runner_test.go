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
