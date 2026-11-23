package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
)

// newJourneyStep makes a journey step that sends one fixed request.
// The target should be a relative path (starting with "/"); the base URL
// will be prepended at runtime by doJourneyStep.
func newJourneyStep(label, target string, check func(int, http.Header, []byte) error, extract func(map[string]any)) JourneyStep {
	return JourneyStep{
		Label: label,
		Render: func(map[string]any) (string, string, http.Header, []byte, error) {
			return "GET", target, nil, nil, nil
		},
		Check: func(_ map[string]any, status int, header http.Header, body []byte) error {
			if check != nil {
				return check(status, header, body)
			}
			return nil
		},
		Extract: func(vars map[string]any, _ http.Header, _ []byte) error {
			if extract != nil {
				extract(vars)
			}
			return nil
		},
	}
}

func journeyLabelsOf(t *testing.T, res *Result, name string) LabelResult {
	t.Helper()
	for _, l := range res.Labels {
		if l.Label == name {
			return l
		}
	}
	t.Fatalf("no label %q in %v", name, res.Labels)
	return LabelResult{}
}

func TestJourneyRecordsSteps(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	jn := &Journey{Name: "test-journey", Steps: []JourneyStep{
		newJourneyStep("alpha", "/a", nil, nil),
		newJourneyStep("beta", "/b", nil, nil),
	}}
	res, err := Run(context.Background(), Config{
		URL: srv.URL, Journey: jn, Rate: 10, Duration: 200 * time.Millisecond, Workers: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Journey == nil {
		t.Fatal("Result.Journey is nil")
	}
	if res.Journey.Name != "test-journey" || res.Journey.Steps != 2 {
		t.Fatalf("journey: %+v", res.Journey)
	}
	if res.Journey.SessionsStarted != res.Scheduled || res.Scheduled == 0 {
		t.Fatalf("started %d, scheduled %d", res.Journey.SessionsStarted, res.Scheduled)
	}
	if res.Journey.SessionsCompleted+res.Journey.SessionsAborted != res.Journey.SessionsStarted {
		t.Fatalf("sessions: started %d completed %d aborted %d",
			res.Journey.SessionsStarted, res.Journey.SessionsCompleted, res.Journey.SessionsAborted)
	}
	if res.Journey.SessionsCompleted == 0 {
		t.Fatalf("no session completed: %+v", res.Journey)
	}
	if res.Journey.JourneyLatencies == nil || res.Journey.JourneyLatencies.Count() == 0 {
		t.Fatal("no journey durations recorded")
	}
	if n := journeyLabelsOf(t, res, "alpha").Scheduled; n == 0 {
		t.Error("step alpha has no scheduled count")
	}
	if n := journeyLabelsOf(t, res, "beta").Scheduled; n == 0 {
		t.Error("step beta has no scheduled count")
	}
	if hits.Load() != int64(res.Journey.SessionsCompleted*2) {
		t.Errorf("server got %d requests, want %d (2 per completed session)", hits.Load(), res.Journey.SessionsCompleted*2)
	}
}

func TestJourneyCheckFailureAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // a bad status, and the check below fails it
	}))
	defer srv.Close()

	jn := &Journey{Name: "j", Steps: []JourneyStep{
		newJourneyStep("one", "/one", func(status int, _ http.Header, _ []byte) error {
			if status != http.StatusOK {
				return errors.New("not ok")
			}
			return nil
		}, nil),
		newJourneyStep("two", "/two", nil, nil),
	}}
	res, err := Run(context.Background(), Config{URL: srv.URL, Journey: jn, Rate: 20, Duration: 200 * time.Millisecond, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Journey.SessionsCompleted != 0 {
		t.Errorf("sessions completed %d, want 0: the check fails every session", res.Journey.SessionsCompleted)
	}
	if res.Journey.Failures["check"] == 0 {
		t.Errorf("no check failures counted: %v", res.Journey.Failures)
	}
	if got := journeyLabelsOf(t, res, "one").BadStatus; got == 0 {
		t.Error("the bad-status step has no bad statuses")
	}
	if n := journeyLabelsOf(t, res, "two").Scheduled; n != 0 {
		t.Errorf("step two ran %d times, want 0: the session aborts at the failed check", n)
	}
}

func TestJourneyContinueOnCheckFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	var continued atomic.Int64
	jn := &Journey{Name: "j", Steps: []JourneyStep{
		{Label: "a", Think: time.Millisecond, ContinueOnCheckFailure: true,
			Render: func(map[string]any) (string, string, http.Header, []byte, error) { return "GET", "/a", nil, nil, nil },
			Check:  func(map[string]any, int, http.Header, []byte) error { return errors.New("fails") }},
		newJourneyStep("b", "/b", nil, func(vars map[string]any) {
			if _, ok := vars["ran_b"]; !ok {
				vars["ran_b"] = true
				continued.Add(1)
			}
		}),
	}}
	res, err := Run(context.Background(), Config{URL: srv.URL, Journey: jn, Rate: 10, Duration: 100 * time.Millisecond, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Journey.Failures["check"] == 0 {
		t.Errorf("no check failures: %v", res.Journey.Failures)
	}
	if res.Journey.SessionsCompleted == 0 {
		t.Error("no sessions completed with continue-on-failure")
	}
}

func TestJourneyExtractionFailureAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	jn := &Journey{Name: "j", Steps: []JourneyStep{
		{Label: "a",
			Render: func(map[string]any) (string, string, http.Header, []byte, error) { return "GET", "/a", nil, nil, nil },
			Extract: func(map[string]any, http.Header, []byte) error { return errors.New("no value") }},
	}}
	res, err := Run(context.Background(), Config{URL: srv.URL, Journey: jn, Rate: 5, Duration: 100 * time.Millisecond, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Journey.SessionsCompleted != 0 || res.Journey.Failures["extraction"] == 0 {
		t.Fatalf("sessions completed %d, failures %v", res.Journey.SessionsCompleted, res.Journey.Failures)
	}
}

func TestJourneyRenderFailureAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	jn := &Journey{Name: "j", Steps: []JourneyStep{
		{Label: "a",
			Render: func(map[string]any) (string, string, http.Header, []byte, error) { return "", "", nil, nil, errors.New("cannot render") }},
	}}
	res, err := Run(context.Background(), Config{URL: srv.URL, Journey: jn, Rate: 5, Duration: 100 * time.Millisecond, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Journey.Failures["render"] == 0 || res.Journey.SessionsAborted == 0 {
		t.Fatalf("render failures %v, aborted %d", res.Journey.Failures, res.Journey.SessionsAborted)
	}
	if n := journeyLabelsOf(t, res, "a").Scheduled; n != 0 {
		t.Errorf("the step was scheduled %d times, want 0", n)
	}
}

func TestJourneyThinkMakesStepOnTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	jn := &Journey{Name: "j", Steps: []JourneyStep{
		newJourneyStep("a", "/a", nil, nil),
		{Label: "b", Think: 60 * time.Millisecond,
			Render: func(map[string]any) (string, string, http.Header, []byte, error) { return "GET", "/b", nil, nil, nil }},
	}}
	res, err := Run(context.Background(), Config{URL: srv.URL, Journey: jn, Rate: 2, Duration: 200 * time.Millisecond, Workers: 4, Phases: true})
	if err != nil {
		t.Fatal(err)
	}
	l := journeyLabelsOf(t, res, "b")
	if l.Scheduled == 0 || l.Corrected == nil {
		t.Fatalf("step b not recorded: %+v", l)
	}
	if mean := l.Corrected.Mean(); mean > 40*time.Millisecond {
		t.Errorf("step b corrected mean %v, over the think time: the step should be due when its think elapses, not late", mean)
	}
	if res.Journey.SessionsCompleted != 0 && res.Journey.SessionsCompleted != res.Journey.SessionsStarted {
		t.Errorf("sessions completed %d of %d", res.Journey.SessionsCompleted, res.Journey.SessionsStarted)
	}
}

func TestJourneyLatencyCountsServerTime(t *testing.T) {
	var delay atomic.Int64
	delay.Store(int64(30 * time.Millisecond))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(time.Duration(delay.Load())); w.WriteHeader(200) }))
	defer srv.Close()

	jn := &Journey{Name: "j", Steps: []JourneyStep{
		newJourneyStep("a", "/a", nil, nil),
	}}
	res, err := Run(context.Background(), Config{URL: srv.URL, Journey: jn, Rate: 1, Duration: 100 * time.Millisecond, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	l := journeyLabelsOf(t, res, "a")
	if l.Corrected.Mean() < 25*time.Millisecond || l.Corrected.Mean() > 45*time.Millisecond {
		t.Errorf("step a corrected mean %v, want about the 30ms server delay", l.Corrected.Mean())
	}
	if gap := l.Corrected.Mean() - l.Service.Mean(); gap > 5*time.Millisecond {
		t.Errorf("step a corrected-minus-service mean %v, want the open-loop queue gap to be ~0", gap)
	}
}

func TestJourneyRenderedAbsoluteURLIsRenderFailure(t *testing.T) {
	var srv2Hits atomic.Int64
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv2Hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv2.Close()

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, srv2.URL+"/x")
	}))
	defer srv1.Close()

	jn := &Journey{Name: "j", Steps: []JourneyStep{
		{Label: "first",
			Render: func(map[string]any) (string, string, http.Header, []byte, error) { return "GET", "/first", nil, nil, nil },
			Extract: func(vars map[string]any, _ http.Header, body []byte) error {
				vars["next"] = string(body)
				return nil
			}},
		{Label: "second",
			Render: func(vars map[string]any) (string, string, http.Header, []byte, error) {
				return "GET", "{{.next}}", nil, nil, nil
			}},
	}}
	// Need path relative? Rendered path will be absolute URL string; should fail.
	// But compile-time? Path template "{{.next}}" has no :// literal, ok.
	res, err := Run(context.Background(), Config{URL: srv1.URL, Journey: jn, Rate: 5, Duration: 100 * time.Millisecond, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if srv2Hits.Load() != 0 {
		t.Fatalf("second server got %d requests, want 0", srv2Hits.Load())
	}
	if res.Journey == nil || res.Journey.Failures["render"] == 0 {
		t.Fatalf("want render failure, got %+v", res.Journey)
	}
}

func TestJourneyNormalizeRefusesBadJourney(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	step := newJourneyStep("a", "/a", nil, nil)
	if _, err := Run(context.Background(), Config{URL: srv.URL, Journey: &Journey{Name: "j", Steps: nil}}); err == nil {
		t.Error("a journey with no steps was accepted")
	}
	dup := &Journey{Name: "j", Steps: []JourneyStep{step, step}}
	if _, err := Run(context.Background(), Config{URL: srv.URL, Journey: dup, Rate: 1, Duration: time.Second}); err == nil {
		t.Error("duplicate step labels were accepted")
	}
	noLabel := JourneyStep{Render: step.Render}
	if _, err := Run(context.Background(), Config{URL: srv.URL, Journey: &Journey{Name: "j", Steps: []JourneyStep{noLabel}}}); err == nil {
		t.Error("an unlabelled step was accepted")
	}
}

func TestJourneyNoStepsHistogramFallback(t *testing.T) {
	// journeyLabels is called for a journey whose steps no session reached.
	jt := map[string]*partTally{"a": {}}
	l := journeyLabels(&Journey{Name: "j", Steps: []JourneyStep{{Label: "a"}, {Label: "b"}}}, jt, true)
	if len(l) != 2 || l[0].Label != "a" {
		t.Fatalf("journeyLabels: %+v", l)
	}
	if l[0].Corrected == nil || l[0].Corrected.Count() != 0 {
		t.Errorf("step with no samples should have an empty histogram: %+v", l[0])
	}
	if l[0].Wait == nil {
		t.Error("step Wait should be non-nil when phases are on")
	}
}

func TestJourneyStatsMerge(t *testing.T) {
	h := hist.New()
	h.Record(time.Second)
	r := func() *Result {
		return &Result{
			Journey:   &JourneyResult{Name: "j", Steps: 1, JourneyLatencies: h, Failures: map[string]uint64{"check": 2}},
			Corrected: hist.New(), Service: hist.New(), TimerLag: hist.New(),
		}
	}
	m, err := Merge(r(), r())
	if err != nil {
		t.Fatal(err)
	}
	if m.Journey == nil {
		t.Fatal("merged journey is nil")
	}
	if got := m.Journey.JourneyLatencies.Count(); got != 2 {
		t.Errorf("merged durations count %d, want 2", got)
	}
	if m.Journey.Failures["check"] != 4 {
		t.Errorf("merged check failures %d, want 4", m.Journey.Failures["check"])
	}
}
