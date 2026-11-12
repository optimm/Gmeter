package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func labelNamed(t *testing.T, res *Result, name string) LabelResult {
	t.Helper()
	for _, l := range res.Labels {
		if l.Label == name {
			return l
		}
	}
	t.Fatalf("no label %q in %v", name, labelsOf(res))
	return LabelResult{}
}

func labelPhase(t *testing.T, l LabelResult, name string) PhaseTotal {
	t.Helper()
	for _, p := range l.Phases {
		if p.Phase == name {
			return p
		}
	}
	t.Fatalf("label %q has no phase %q in %v", l.Label, name, l.Phases)
	return PhaseTotal{}
}

// Each label's phases split its own requests' corrected latency exactly, and
// the labels together split the whole run's.
func TestLabelPhaseTotalsSplitEachLabelsLatencyAndAddUpToTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(50 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := baseConfig(srv.URL) // 100 requests, 50 to each label, up to 8 in flight
	cfg.Requests = labelled("/slow", "/fast")
	cfg.Phases = true

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	slow, fast := labelNamed(t, res, "/slow"), labelNamed(t, res, "/fast")
	for _, l := range []LabelResult{slow, fast} {
		if len(l.Phases) != len(PhaseNames) {
			t.Fatalf("%s has %d phases, want %d", l.Label, len(l.Phases), len(PhaseNames))
		}
		var total time.Duration
		for i, p := range l.Phases {
			if p.Phase != PhaseNames[i] {
				t.Errorf("%s phase %d is %q, want %q", l.Label, i, p.Phase, PhaseNames[i])
			}
			total += p.Total
		}
		if total != l.Corrected.Sum() {
			t.Errorf("%s: the phases add up to %v but its corrected latencies add up to %v", l.Label, total, l.Corrected.Sum())
		}
		for _, name := range []string{"queue", "send", "wait", "transfer"} {
			if n := labelPhase(t, l, name).Requests; n != l.Corrected.Count() {
				t.Errorf("%s %s has %d requests, want one per response (%d)", l.Label, name, n, l.Corrected.Count())
			}
		}
	}

	if got := labelPhase(t, slow, "wait").Total / time.Duration(slow.Corrected.Count()); got < 45*time.Millisecond {
		t.Errorf("/slow waits %v per request, want about the server's 50ms", got)
	}
	if got := labelPhase(t, fast, "wait").Total / time.Duration(fast.Corrected.Count()); got > 25*time.Millisecond {
		t.Errorf("/fast waits %v per request, want far less than /slow's 50ms", got)
	}

	for i, p := range res.Phases {
		var requests uint64
		var total time.Duration
		for _, l := range res.Labels {
			requests += l.Phases[i].Requests
			total += l.Phases[i].Total
		}
		if requests != p.Samples.Count() || total != p.Samples.Sum() {
			t.Errorf("%s: labels have %d requests totalling %v, the run has %d totalling %v",
				p.Phase, requests, total, p.Samples.Count(), p.Samples.Sum())
		}
	}
}

// The requests that waited behind a slow label for a worker show it as queue,
// and only as queue: that is the difference between the generator being the
// bottleneck and the server being it.
func TestALabelThatWaitedForAWorkerShowsItAsQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(100 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := offsetsConfig(srv.URL, 0, 20*time.Millisecond, 40*time.Millisecond, 60*time.Millisecond)
	cfg.Workers = 1
	cfg.Requests = labelled("/slow", "/fast", "/slow", "/fast")
	cfg.Phases = true

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	fast := labelNamed(t, res, "/fast")
	if got := labelPhase(t, fast, "queue").Total / 2; got < 60*time.Millisecond {
		t.Errorf("/fast queued %v per request, want at least 60ms: each waited for a 100ms /slow request", got)
	}
	if got := labelPhase(t, fast, "wait").Total / 2; got > 50*time.Millisecond {
		t.Errorf("/fast waited %v per request, want little: once sent it was answered at once", got)
	}
}

func TestLabelsHaveNoPhasesUnlessAskedFor(t *testing.T) {
	srv, _ := recordingServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Requests = labelled("/a", "/b")

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.Labels {
		if l.Phases != nil {
			t.Errorf("%s: Phases = %v, want nil when Config.Phases is not set", l.Label, l.Phases)
		}
	}
}

// A label none of whose requests got a response still reports every phase, all
// of them empty.
func TestALabelWithoutAResponseHasEmptyPhases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/drop" { // close the connection without answering
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}
	}))
	t.Cleanup(srv.Close)
	cfg := baseConfig(srv.URL)
	cfg.Requests = labelled("/ok", "/drop")
	cfg.Phases = true

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	drop := labelNamed(t, res, "/drop")
	if drop.Failed == 0 || drop.Corrected.Count() != 0 {
		t.Fatalf("/drop: failed %d, %d samples, want failures and no samples", drop.Failed, drop.Corrected.Count())
	}
	if len(drop.Phases) != len(PhaseNames) {
		t.Fatalf("/drop has %d phases, want %d", len(drop.Phases), len(PhaseNames))
	}
	for _, p := range drop.Phases {
		if p.Requests != 0 || p.Total != 0 {
			t.Errorf("/drop %s = %d requests, %v: want empty", p.Phase, p.Requests, p.Total)
		}
	}
}
