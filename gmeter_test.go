package gmeter_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter"
)

func server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func ok(t *testing.T) *httptest.Server {
	return server(t, func(w http.ResponseWriter, r *http.Request) {})
}

func TestRunSendsTheRateForTheDurationAndCountsWhatCameBack(t *testing.T) {
	srv := ok(t)

	res, err := gmeter.Run(context.Background(), gmeter.Config{URL: srv.URL, Rate: 200, Duration: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	if res.Scheduled() != 100 || res.Responses() != 100 || res.OK() != 100 || res.Failed() != 0 || res.BadStatus() != 0 {
		t.Errorf("scheduled %d, responses %d, ok %d, failed %d, bad status %d; want 100, 100, 100, 0, 0",
			res.Scheduled(), res.Responses(), res.OK(), res.Failed(), res.BadStatus())
	}
	if res.Statuses()[200] != 100 || res.ErrorRate() != 0 {
		t.Errorf("statuses %v, error rate %v", res.Statuses(), res.ErrorRate())
	}
	if p50, p99 := res.Corrected(0.5), res.Corrected(0.99); p50 <= 0 || p99 < p50 || res.Service(0.99) <= 0 {
		t.Errorf("corrected p50 %v p99 %v, service p99 %v: want positive and ordered", p50, p99, res.Service(0.99))
	}
	if res.Elapsed() < 450*time.Millisecond {
		t.Errorf("elapsed %v for a half-second run", res.Elapsed())
	}
}

func TestRunWithStagesFollowsTheProfile(t *testing.T) {
	srv := ok(t)

	res, err := gmeter.Run(context.Background(), gmeter.Config{URL: srv.URL, Stages: []gmeter.Stage{
		{Duration: 200 * time.Millisecond, From: 100, To: 100},
		{Duration: 200 * time.Millisecond, From: 200, To: 200},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if res.Scheduled() != 60 { // 20 at 100/s, then 40 at 200/s
		t.Errorf("scheduled %d, want 60", res.Scheduled())
	}
}

func TestRunSendsEachRequestOfASetInTurn(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Who")+" "+r.Header.Get("X-Common")]++
		mu.Unlock()
	})

	res, err := gmeter.Run(context.Background(), gmeter.Config{
		URL: srv.URL, Rate: 100, Duration: 200 * time.Millisecond,
		Header: http.Header{"X-Common": {"all"}},
		Requests: []gmeter.Request{
			{Path: "/read", Label: "read"},
			{Method: "POST", Path: "/write?x=1", Header: http.Header{"X-Who": {"writer"}}, Body: []byte("hi"), Label: "write"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.Scheduled() != 20 || got["GET /read  all"] != 10 || got["POST /write writer all"] != 10 {
		t.Errorf("scheduled %d, the server saw %v; want 10 of each request", res.Scheduled(), got)
	}
}

func TestRunWithTheSameSeedSendsTheSameArrivals(t *testing.T) {
	srv := ok(t)
	run := func() uint64 {
		res, err := gmeter.Run(context.Background(), gmeter.Config{URL: srv.URL, Rate: 300, Duration: 300 * time.Millisecond, Poisson: true, Seed: 7})
		if err != nil {
			t.Fatal(err)
		}
		return res.Scheduled()
	}
	if a, b := run(), run(); a != b {
		t.Errorf("two runs with seed 7 scheduled %d and %d requests", a, b)
	}
}

func TestOnRequestSeesEveryRequest(t *testing.T) {
	srv := ok(t)
	var n atomic.Int64

	res, err := gmeter.Run(context.Background(), gmeter.Config{URL: srv.URL, Rate: 100, Duration: 200 * time.Millisecond,
		OnRequest: func(s gmeter.Sample) {
			if s.Status == 200 && s.Finished >= s.Sent && s.Sent >= s.Due {
				n.Add(1)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if uint64(n.Load()) != res.Scheduled() || res.Scheduled() != 20 {
		t.Errorf("saw %d well-formed samples for %d requests, want 20", n.Load(), res.Scheduled())
	}
}

func TestRunHonoursTheTLSConfig(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // a refused handshake is the point of this test
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cfg := gmeter.Config{URL: srv.URL, Rate: 50, Duration: 200 * time.Millisecond}

	res, err := gmeter.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors()["tls"] != 10 || res.OK() != 0 {
		t.Errorf("without trust: errors %v, ok %d; want 10 tls failures", res.Errors(), res.OK())
	}

	cfg.TLS = &tls.Config{InsecureSkipVerify: true}
	if res, err = gmeter.Run(context.Background(), cfg); err != nil || res.OK() != 10 {
		t.Errorf("with TLS config: err %v, ok %d, want 10", err, res.OK())
	}
}

func TestRunRefusesAConfigItCannotRun(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  gmeter.Config
		want string
	}{
		"no URL":           {gmeter.Config{Rate: 10, Duration: time.Second}, "URL"},
		"no load":          {gmeter.Config{URL: "http://x"}, "Rate"},
		"rate and stages":  {gmeter.Config{URL: "http://x", Rate: 10, Duration: time.Second, Stages: []gmeter.Stage{{Duration: time.Second, From: 1, To: 1}}}, "Stages"},
		"negative workers": {gmeter.Config{URL: "http://x", Rate: 10, Duration: time.Second, Workers: -1}, "orkers"},
		"bad path":         {gmeter.Config{URL: "http://x", Rate: 10, Duration: time.Second, Requests: []gmeter.Request{{Path: "no-slash"}}}, "path"},
	} {
		_, err := gmeter.Run(context.Background(), tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, tc.want)
		}
	}
}

func TestMeetsSaysWhyATargetWasMissed(t *testing.T) {
	slow := server(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(30 * time.Millisecond) })
	broken := server(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	fast := ok(t)
	run := func(srv *httptest.Server) *gmeter.Result {
		res, err := gmeter.Run(context.Background(), gmeter.Config{URL: srv.URL, Rate: 50, Duration: 300 * time.Millisecond, Workers: 20})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	target := gmeter.SLO{Quantile: 0.99, Latency: 10 * time.Millisecond}

	if err := run(fast).Meets(target); err != nil {
		t.Errorf("a fast server missed the target: %v", err)
	}
	if err := run(slow).Meets(target); err == nil || !strings.Contains(err.Error(), "p99") || !strings.Contains(err.Error(), "10ms") {
		t.Errorf("a slow server: err = %v, want one naming p99 and the 10ms target", err)
	}
	if err := run(broken).Meets(target); err == nil || !strings.Contains(err.Error(), "errors") {
		t.Errorf("a server answering 500: err = %v, want one about errors", err)
	}
	allowed := target
	allowed.MaxErrorRate = 1
	if err := run(broken).Meets(allowed); err != nil {
		t.Errorf("with every error allowed: %v", err)
	}
}

func TestMeetsRefusesATargetThatMakesNoSense(t *testing.T) {
	res, err := gmeter.Run(context.Background(), gmeter.Config{URL: ok(t).URL, Rate: 20, Duration: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for name, slo := range map[string]gmeter.SLO{
		"no latency":       {Quantile: 0.99},
		"quantile too big": {Quantile: 1.5, Latency: time.Second},
		"no quantile":      {Latency: time.Second},
		"bad error rate":   {Quantile: 0.99, Latency: time.Second, MaxErrorRate: 2},
	} {
		if err := res.Meets(slo); err == nil || errors.Is(err, gmeter.ErrTargetMissed) {
			t.Errorf("%s: err = %v, want a complaint about the target itself", name, err)
		}
	}
}

func TestAMissedTargetIsAnErrTargetMissed(t *testing.T) {
	res, err := gmeter.Run(context.Background(), gmeter.Config{URL: server(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }).URL, Rate: 20, Duration: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Meets(gmeter.SLO{Quantile: 0.99, Latency: time.Second}); !errors.Is(err, gmeter.ErrTargetMissed) {
		t.Errorf("err = %v, want it to be ErrTargetMissed", err)
	}
}

func TestParseSLOReadsTheCommandLineSpelling(t *testing.T) {
	slo, err := gmeter.ParseSLO("p99.9=250ms")
	if err != nil || slo.Quantile != 0.999 || slo.Latency != 250*time.Millisecond || slo.MaxErrorRate != 0 {
		t.Errorf("got %+v, %v", slo, err)
	}
	for _, bad := range []string{"", "99ms", "p0=1s", "p100=1s", "p99=fast", "p99=-1s"} {
		if _, err := gmeter.ParseSLO(bad); err == nil {
			t.Errorf("ParseSLO(%q) accepted it", bad)
		}
	}
}

func TestAResultWritesTheSameReportsAsTheCommand(t *testing.T) {
	res, err := gmeter.Run(context.Background(), gmeter.Config{URL: ok(t).URL, Rate: 50, Duration: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	var text, js bytes.Buffer
	if err := res.WriteText(&text); err != nil || !strings.Contains(text.String(), "10 scheduled") || !strings.Contains(text.String(), "latency, corrected") {
		t.Errorf("text report (%v):\n%s", err, text.String())
	}
	var parsed struct {
		Scheduled uint64  `json:"scheduled"`
		TargetRPS float64 `json:"target_rps"`
	}
	if err := res.WriteJSON(&js); err != nil || json.Unmarshal(js.Bytes(), &parsed) != nil || parsed.Scheduled != 10 || parsed.TargetRPS != 50 {
		t.Errorf("JSON report (%v): %+v\n%s", err, parsed, js.String())
	}
}

func TestFindCapacityReportsTheHighestRateThatMeetsTheTarget(t *testing.T) {
	srv := ok(t)

	got, err := gmeter.FindCapacity(context.Background(),
		gmeter.Config{URL: srv.URL, Workers: 20},
		gmeter.SLO{Quantile: 0.99, Latency: time.Second},
		gmeter.SearchOptions{Start: 20, Max: 40, Trial: 300 * time.Millisecond, Warmup: -1})
	if err != nil {
		t.Fatal(err)
	}

	if !got.Found || got.MaxRPS != 40 || !got.LowerBound || len(got.Trials) < 2 {
		t.Errorf("got %+v, want 40 rps found as a lower bound after at least two trials", got)
	}
	for _, tr := range got.Trials {
		if tr.Verdict != "pass" || tr.Scheduled == 0 {
			t.Errorf("trial %+v: want a pass with requests sent", tr)
		}
	}
}

func TestFindCapacityOfAServerThatNeverMeetsTheTargetFindsNothing(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })

	got, err := gmeter.FindCapacity(context.Background(),
		gmeter.Config{URL: srv.URL, Workers: 10},
		gmeter.SLO{Quantile: 0.99, Latency: time.Second},
		gmeter.SearchOptions{Start: 8, Max: 16, Trial: 200 * time.Millisecond, Warmup: -1})
	if err != nil {
		t.Fatal(err)
	}

	if got.Found || got.MaxRPS != 0 || got.Trials[0].Verdict != "fail" || len(got.Trials[0].Reasons) == 0 {
		t.Errorf("got %+v, want nothing found, with the reason the first trial failed", got)
	}
}

func TestFindCapacityStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := gmeter.FindCapacity(ctx, gmeter.Config{URL: ok(t).URL},
		gmeter.SLO{Quantile: 0.99, Latency: time.Second},
		gmeter.SearchOptions{Start: 10, Max: 1000, Trial: 5 * time.Second})

	if !errors.Is(err, context.Canceled) || time.Since(start) > 3*time.Second {
		t.Errorf("err = %v after %v, want context.Canceled soon after the cancel", err, time.Since(start))
	}
}

func TestFindCapacityRefusesOptionsItCannotSearchWith(t *testing.T) {
	slo := gmeter.SLO{Quantile: 0.99, Latency: time.Second}
	cfg := gmeter.Config{URL: "http://x"}
	for name, opts := range map[string]gmeter.SearchOptions{
		"max below start": {Start: 100, Max: 10},
		"bad tolerance":   {Tolerance: 5},
	} {
		if _, err := gmeter.FindCapacity(context.Background(), cfg, slo, opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := gmeter.FindCapacity(context.Background(), cfg, gmeter.SLO{}, gmeter.SearchOptions{}); err == nil {
		t.Error("a zero SLO was accepted")
	}
}
