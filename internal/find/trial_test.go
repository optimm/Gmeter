package find

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

func baseConfig(url string) runner.Config {
	return runner.Config{URL: url, Method: "GET", Workers: 32, Timeout: 2 * time.Second, Spin: 200 * time.Microsecond}
}

func server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

var target = SLO{Quantile: 0.99, Latency: 40 * time.Millisecond, MaxErrorRate: 0.01}

func runTrial(t *testing.T, srv *httptest.Server, slo SLO, warmup time.Duration) (Verdict, Observation) {
	t.Helper()
	v, o, err := RunTrial(context.Background(), baseConfig(srv.URL), slo, warmup, 400*time.Millisecond, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if v == Limited {
		// Correct behaviour, but it says nothing about what is being tested here:
		// these trials last under a second, so a single timer hiccup is their p99.
		t.Skipf("this machine's timers ran late (%v), so the trial was rightly not trusted", o.Reasons)
	}
	return v, o
}

func TestATrialAgainstAHealthyServerPasses(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {})
	v, o := runTrial(t, srv, target, 100*time.Millisecond)
	if v != Pass {
		t.Fatalf("verdict %v, reasons %v, want a pass", v, o.Reasons)
	}
	if o.Scheduled != 40 || o.Rate != 100 { // 100 rps for the 400ms measured; the warm-up is not counted
		t.Errorf("observation %+v: want 40 requests at 100 rps", o)
	}
}

func TestATrialAgainstASlowServerFails(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(80 * time.Millisecond) })
	if v, o := runTrial(t, srv, target, 100*time.Millisecond); v != Fail || o.Observed < 80*time.Millisecond {
		t.Fatalf("verdict %v, p99 %v: want a failure at 80ms or more", v, o.Observed)
	}
}

func TestATrialAgainstAServerThatReturnsErrorsFails(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if v, o := runTrial(t, srv, target, 100*time.Millisecond); v != Fail || o.ErrorRate < 0.99 {
		t.Fatalf("verdict %v, error rate %v: want a failure at ~100%% errors", v, o.ErrorRate)
	}
}

// Connections, caches and JITs are cold at the start of a trial. The warm-up
// takes that cost, and only the measured stage is judged.
func TestTheWarmUpIsNotPartOfTheVerdict(t *testing.T) {
	begin := time.Now()
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if time.Since(begin) < 200*time.Millisecond { // slow while "warming up"
			time.Sleep(100 * time.Millisecond)
		}
	})
	v, o := runTrial(t, srv, target, 300*time.Millisecond)
	if v != Pass {
		t.Fatalf("verdict %v (p99 %v, reasons %v): the slow requests were all in the warm-up and must not count", v, o.Observed, o.Reasons)
	}
}

func TestATrialWithoutAWarmUpMeasuresEverything(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {})
	v, o := runTrial(t, srv, target, 0)
	if v != Pass || o.Scheduled != 40 {
		t.Fatalf("verdict %v with %d requests, want a pass over 40 requests", v, o.Scheduled)
	}
}

func TestATrialReportsConfigErrors(t *testing.T) {
	cfg := baseConfig("ftp://nope")
	if _, _, err := RunTrial(context.Background(), cfg, target, 0, time.Second, 1, 100); err == nil {
		t.Fatal("a bad URL should be an error and not a verdict")
	}
}
