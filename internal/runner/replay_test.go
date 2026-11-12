package runner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
)

// seen is what a server observed about one request.
type seen struct {
	method, uri, body string
	header            http.Header
	arrived           time.Time
}

// recordingServer answers 200 to everything and remembers what it was sent.
func recordingServer(t *testing.T) (*httptest.Server, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.Method, r.RequestURI, string(b), r.Header.Clone(), time.Now()})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), got...)
	}
}

func offsetsConfig(url string, offsets ...time.Duration) Config {
	return Config{URL: url, Offsets: offsets, Workers: 4, Timeout: 2 * time.Second}
}

func TestEachReplayedRequestSendsItsOwnMethodTargetHeadersAndBody(t *testing.T) {
	srv, got := recordingServer(t)
	cfg := offsetsConfig(srv.URL, 0, 40*time.Millisecond)
	cfg.Method = "PUT" // what a request that does not say falls back to
	cfg.Requests = []Request{
		{Target: "/a?x=1&y=2"},
		{Method: "POST", Target: "/b", Body: []byte("hello"), Header: http.Header{"Content-Type": {"text/plain"}}},
	}

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 2 || res.Statuses[200] != 2 {
		t.Fatalf("Scheduled = %d, statuses = %v, want 2 requests answered 200", res.Scheduled, res.Statuses)
	}
	reqs := got()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(reqs))
	}
	if r := reqs[0]; r.method != "PUT" || r.uri != "/a?x=1&y=2" || r.body != "" {
		t.Errorf("first request = %s %s %q, want PUT /a?x=1&y=2 with no body", r.method, r.uri, r.body)
	}
	if r := reqs[1]; r.method != "POST" || r.uri != "/b" || r.body != "hello" || r.header.Get("Content-Type") != "text/plain" {
		t.Errorf("second request = %s %s %q content-type %q, want POST /b \"hello\" text/plain",
			r.method, r.uri, r.body, r.header.Get("Content-Type"))
	}
}

func TestReplayedRequestsCycleWhenTheScheduleIsLongerThanTheTrace(t *testing.T) {
	srv, got := recordingServer(t)
	cfg := baseConfig(srv.URL) // 200 rps for 0.5s: 100 requests
	cfg.Requests = []Request{{Target: "/a"}, {Target: "/b"}, {Target: "/c"}, {Target: "/d"}}

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 100 {
		t.Fatalf("Scheduled = %d, want 100", res.Scheduled)
	}
	count := map[string]int{}
	for _, r := range got() {
		count[r.uri]++
	}
	for _, p := range []string{"/a", "/b", "/c", "/d"} {
		if count[p] != 25 {
			t.Errorf("%s was requested %d times, want 25 (100 requests over 4 targets): %v", p, count[p], count)
		}
	}
}

func TestReplayedRequestHeadersAreAddedToTheGlobalOnesAndWinOnConflict(t *testing.T) {
	srv, got := recordingServer(t)
	cfg := offsetsConfig(srv.URL, 0, time.Millisecond)
	cfg.Header = http.Header{"Authorization": {"Bearer t"}, "X-Env": {"base"}}
	cfg.Requests = []Request{{Target: "/", Header: http.Header{"X-Env": {"replay"}, "X-Only": {"1"}}}}

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	h := got()[0].header
	if h.Get("Authorization") != "Bearer t" {
		t.Errorf("Authorization = %q: the global header must still be sent", h.Get("Authorization"))
	}
	if v := h.Values("X-Env"); len(v) != 1 || v[0] != "replay" {
		t.Errorf("X-Env = %q, want only the request's own value", v)
	}
	if h.Get("X-Only") != "1" {
		t.Errorf("X-Only = %q, want 1", h.Get("X-Only"))
	}
}

func TestReplayedTargetsAreAppendedToTheBaseURLPath(t *testing.T) {
	srv, got := recordingServer(t)
	cfg := offsetsConfig(srv.URL+"/api/", 0, time.Millisecond)
	cfg.Requests = []Request{{Target: "/users?id=7"}}

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if uri := got()[0].uri; uri != "/api/users?id=7" {
		t.Errorf("request URI = %q, want /api/users?id=7", uri)
	}
}

func TestRecordedOffsetsDecideWhenRequestsAreDue(t *testing.T) {
	srv, got := recordingServer(t)
	offsets := []time.Duration{0, 80 * time.Millisecond, 80 * time.Millisecond, 250 * time.Millisecond}
	cfg := offsetsConfig(srv.URL, offsets...)
	cfg.Spin = 200 * time.Microsecond

	started := time.Now()
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 4 || res.Dropped != 0 {
		t.Fatalf("Scheduled = %d, Dropped = %d, want 4 and 0", res.Scheduled, res.Dropped)
	}
	if len(res.Stages) != 1 || res.Stages[0].Scheduled != 4 {
		t.Fatalf("stages = %+v, want a single stage with all 4 requests", res.Stages)
	}
	reqs := got()
	if len(reqs) != 4 {
		t.Fatalf("server saw %d requests, want 4", len(reqs))
	}
	for i, r := range reqs {
		at := r.arrived.Sub(started)
		// Never early; and on a quiet machine within a few ms. The upper bound is
		// loose because this is a timing test on a shared CPU.
		if at < offsets[i]-5*time.Millisecond || at > offsets[i]+120*time.Millisecond {
			t.Errorf("request %d arrived %v after the start, want about %v", i, at, offsets[i])
		}
	}
}

// The property the whole tool exists for, for a replay: a recorded request that
// was due during a stall is measured from its recorded time, not from when the
// server got round to it.
func TestReplayStaysOpenLoopWhenTheServerStalls(t *testing.T) {
	var first atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			time.Sleep(300 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := offsetsConfig(srv.URL, 0, 50*time.Millisecond, 100*time.Millisecond, 150*time.Millisecond, 200*time.Millisecond)
	cfg.Workers = 1
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Corrected: ~300, 250, 200, 150, 100ms. Service: one 300ms and four fast.
	if p50 := res.Corrected.Quantile(0.5); p50 < 150*time.Millisecond {
		t.Errorf("corrected p50 = %v, want about 200ms: the requests due during the stall must show it", p50)
	}
	if p50 := res.Service.Quantile(0.5); p50 > 50*time.Millisecond {
		t.Errorf("service p50 = %v, want a few ms: only one request was actually slow", p50)
	}
}

func TestRejectsInvalidReplayConfig(t *testing.T) {
	srv, _ := recordingServer(t)
	ok := func() Config {
		c := offsetsConfig(srv.URL, 0, time.Millisecond)
		c.Requests = []Request{{Target: "/a"}}
		return c
	}
	for name, tc := range map[string]struct {
		change func(*Config)
		want   string
	}{
		"target without a leading slash": {func(c *Config) { c.Requests[0].Target = "a" }, "target"},
		"absolute target":                {func(c *Config) { c.Requests[0].Target = "http://elsewhere.example/a" }, "target"},
		"empty target":                   {func(c *Config) { c.Requests[0].Target = "" }, "target"},
		"base url with a query":          {func(c *Config) { c.URL += "?a=1" }, "query"},
		"bad method":                     {func(c *Config) { c.Requests[0].Method = "BAD METHOD" }, "method"},
		"offsets and a rate":             {func(c *Config) { c.Rate, c.Duration = 10, time.Second }, "Offsets"},
		"offsets and stages":             {func(c *Config) { c.Stages = pacer.ConstantRate(10, time.Second) }, "Offsets"},
		"offsets and poisson":            {func(c *Config) { c.Arrivals = pacer.Poisson }, "Offsets"},
		"offsets and a seed":             {func(c *Config) { c.Seed = 5 }, "Offsets"},
		"unsorted offsets":               {func(c *Config) { c.Offsets = []time.Duration{time.Second, 0} }, "order"},
		"offsets that span no time":      {func(c *Config) { c.Offsets = []time.Duration{0, 0} }, "no time"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := ok()
			tc.change(&cfg)
			_, err := Run(context.Background(), cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
