package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func phaseNamed(t *testing.T, res *Result, name string) PhaseResult {
	t.Helper()
	for _, p := range res.Phases {
		if p.Phase == name {
			return p
		}
	}
	t.Fatalf("no phase %q in %v", name, res.Phases)
	return PhaseResult{}
}

func TestRunSplitsEveryResponseIntoPhasesThatAddUpToItsCorrectedLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	cfg := baseConfig(srv.URL) // 100 requests, up to 8 in flight
	cfg.Phases = true

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Phases) != len(PhaseNames) {
		t.Fatalf("%d phases, want %d", len(res.Phases), len(PhaseNames))
	}
	for i, p := range res.Phases {
		if p.Phase != PhaseNames[i] {
			t.Errorf("phase %d is %q, want %q: in the order a request goes through them", i, p.Phase, PhaseNames[i])
		}
	}
	responses := res.Corrected.Count()
	for _, name := range []string{"queue", "send", "wait", "transfer"} {
		if n := phaseNamed(t, res, name).Samples.Count(); n != responses {
			t.Errorf("%s has %d samples, want one per response (%d)", name, n, responses)
		}
	}
	if n := phaseNamed(t, res, "connect").Samples.Count(); n < 1 || n > uint64(cfg.Workers) {
		t.Errorf("connect has %d samples, want one per connection opened: between 1 and %d", n, cfg.Workers)
	}
	for _, name := range []string{"dns", "tls"} {
		if n := phaseNamed(t, res, name).Samples.Count(); n != 0 {
			t.Errorf("%s has %d samples, want none: an IP address over plain HTTP", name, n)
		}
	}
	if p50 := phaseNamed(t, res, "wait").Samples.Quantile(0.5); p50 < 19*time.Millisecond {
		t.Errorf("wait p50 = %v, want at least the server's 20ms", p50)
	}

	var total time.Duration
	for _, p := range res.Phases {
		total += p.Samples.Sum()
	}
	if total != res.Corrected.Sum() {
		t.Errorf("the phases add up to %v but the corrected latencies add up to %v: they must split it exactly", total, res.Corrected.Sum())
	}
}

func TestPhasesAreOnlyRecordedWhenAskedFor(t *testing.T) {
	srv, _ := recordingServer(t)

	res, err := Run(context.Background(), baseConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Phases != nil {
		t.Errorf("Phases = %v, want nil when Config.Phases is not set", res.Phases)
	}
}

func TestARequestThatGotNoResponseHasNoPhases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}))
	t.Cleanup(srv.Close)
	cfg := offsetsConfig(srv.URL, 0, 10*time.Millisecond, 20*time.Millisecond)
	cfg.Timeout = 100 * time.Millisecond
	cfg.Phases = true

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors["timeout"] != 3 {
		t.Fatalf("errors = %v, want 3 timeouts", res.Errors)
	}
	if res.Phases == nil {
		t.Fatal("Phases is nil although they were asked for")
	}
	for _, p := range res.Phases {
		if n := p.Samples.Count(); n != 0 {
			t.Errorf("%s has %d samples, want none: a timeout is not a response to split", p.Phase, n)
		}
	}
}

// With one worker and a slow server, requests wait for the worker: that wait
// is the queue phase, and nothing else grows.
func TestQueueIsTheTimeAWorkerWasBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	cfg := offsetsConfig(srv.URL, 0, 20*time.Millisecond, 40*time.Millisecond)
	cfg.Workers = 1
	cfg.Phases = true

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The third request was due at 40ms and could only start when the second,
	// started at about 100ms, finished at about 200ms.
	if got := phaseNamed(t, res, "queue").Samples.Max(); got < 100*time.Millisecond {
		t.Errorf("queue max = %v, want at least 100ms", got)
	}
	if got := phaseNamed(t, res, "wait").Samples.Max(); got > 190*time.Millisecond {
		t.Errorf("wait max = %v, want about the server's 100ms: the queue is not the server's time", got)
	}
}
