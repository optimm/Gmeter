package runner

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// callConfig runs call at 100 per second for 300ms: 30 calls.
func callConfig(call func(context.Context) (string, error)) Config {
	return Config{Rate: 100, Duration: 300 * time.Millisecond, Workers: 4, Timeout: time.Second, Call: call}
}

func TestACallIsTimedFromItsDueTimeLikeARequest(t *testing.T) {
	cfg := callConfig(func(ctx context.Context) (string, error) {
		time.Sleep(10 * time.Millisecond)
		return "OK", nil
	})
	cfg.Workers = 1 // one at a time, 10ms each, at most one every 10ms: they queue a little

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 30 || res.Codes["OK"] != 30 || len(res.Statuses) != 0 {
		t.Errorf("scheduled %d, codes %v, statuses %v; want 30 OK and no HTTP status", res.Scheduled, res.Codes, res.Statuses)
	}
	if res.Corrected.Count() != 30 || res.Corrected.Quantile(0.5) < 10*time.Millisecond || res.Service.Quantile(0.5) < 10*time.Millisecond {
		t.Errorf("corrected: %d samples, p50 %v, service p50 %v; want 30 of at least 10ms", res.Corrected.Count(), res.Corrected.Quantile(0.5), res.Service.Quantile(0.5))
	}
	if res.Stages[0].BadStatus != 0 || res.Stages[0].Failed != 0 {
		t.Errorf("stage = %+v, want nothing bad", res.Stages[0])
	}
	var answered uint64
	for _, w := range res.Timeline {
		answered += w.Responses
	}
	if answered != 30 {
		t.Errorf("the timeline has %d answers, want 30", answered)
	}
}

func TestOnlyAnOKCallIsOK(t *testing.T) {
	var n atomic.Int64
	res, err := Run(context.Background(), callConfig(func(context.Context) (string, error) {
		if n.Add(1)%3 == 0 {
			return "NOT_FOUND", nil
		}
		return "OK", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Codes["OK"] != 20 || res.Codes["NOT_FOUND"] != 10 || res.Stages[0].BadStatus != 10 || res.Corrected.Count() != 30 {
		t.Errorf("codes %v, bad %d, %d answers timed; want 20 OK and 10 NOT_FOUND, all timed, the 10 bad",
			res.Codes, res.Stages[0].BadStatus, res.Corrected.Count())
	}
}

func TestACallThatGetsNoAnswerIsAFailure(t *testing.T) {
	res, err := Run(context.Background(), callConfig(func(context.Context) (string, error) {
		return "", Failure{Kind: "unavailable", Err: errors.New("connection refused")}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors["unavailable"] != 30 || res.Stages[0].Failed != 30 || res.Corrected.Count() != 0 || len(res.Codes) != 0 {
		t.Errorf("errors %v, failed %d, %d timed, codes %v; want 30 unavailable failures, none timed",
			res.Errors, res.Stages[0].Failed, res.Corrected.Count(), res.Codes)
	}

	// A call that outlives the timeout is a timeout, and timed: a hung server must not look fast.
	cfg := callConfig(func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	cfg.Timeout, cfg.Workers = 20*time.Millisecond, 30
	res, err = Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors["timeout"] != 30 || res.Corrected.Count() != 30 || res.Corrected.Quantile(0.5) < 20*time.Millisecond {
		t.Errorf("errors %v, %d timed, p50 %v; want 30 timeouts timed at 20ms or more", res.Errors, res.Corrected.Count(), res.Corrected.Quantile(0.5))
	}
}

func TestStoppingARunCancelsItsCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int64
	cfg := callConfig(func(c context.Context) (string, error) {
		if started.Add(1) == 4 {
			cancel()
		}
		<-c.Done()
		return "", c.Err()
	})
	cfg.Duration = 10 * time.Second
	res, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors["canceled"] < 4 || res.Errors["timeout"] != 0 || res.Elapsed > 5*time.Second {
		t.Errorf("errors %v after %v, want the calls canceled at once", res.Errors, res.Elapsed)
	}
}

func TestACallLeavesNoRoomForHTTPSettings(t *testing.T) {
	ok := func(context.Context) (string, error) { return "OK", nil }
	if err := callConfig(ok).Validate(); err != nil {
		t.Fatalf("a call without a URL: %v", err)
	}
	for name, change := range map[string]func(*Config){
		"Method":   func(c *Config) { c.Method = http.MethodPost },
		"Header":   func(c *Config) { c.Header = http.Header{"X": {"y"}} },
		"Body":     func(c *Config) { c.Body = []byte("x") },
		"Requests": func(c *Config) { c.Requests = []Request{{Target: "/"}} },
		"TLS":      func(c *Config) { c.TLS = &tls.Config{} },
		"HTTP2":    func(c *Config) { c.HTTP2 = true },
		"Phases":   func(c *Config) { c.Phases = true },
	} {
		cfg := callConfig(ok)
		change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was taken with a Call, which would ignore it", name)
		}
	}
}

func TestMergeAddsUpCallCodes(t *testing.T) {
	a := generatorResult(10, time.Millisecond, 0, "x", time.Second)
	b := generatorResult(10, time.Millisecond, 0, "x", time.Second)
	a.Codes, b.Codes = map[string]uint64{"OK": 3, "INTERNAL": 1}, map[string]uint64{"OK": 2}
	m, err := Merge(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Codes["OK"] != 5 || m.Codes["INTERNAL"] != 1 {
		t.Errorf("codes = %v, want 5 OK and 1 INTERNAL", m.Codes)
	}
}

func TestTheProgressLineCountsAnsweredCalls(t *testing.T) {
	var completed atomic.Uint64
	cfg := callConfig(func(context.Context) (string, error) { return "INTERNAL", nil })
	cfg.SampleEvery = 50 * time.Millisecond
	cfg.Progress = func(s Snapshot) { completed.Store(max(completed.Load(), s.Completed)) }
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if completed.Load() < 10 {
		t.Errorf("progress saw %d calls answered, want most of the 30", completed.Load())
	}
}
