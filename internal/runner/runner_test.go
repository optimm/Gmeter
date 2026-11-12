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
	cfg.ProgressEvery = 100 * time.Millisecond

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
