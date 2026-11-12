package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/report"
)

// failingServer answers every request with a 500 at once.
func failingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A server that fails at once answers faster than one that works, and a bad
// status is recorded as latency: the p99 gate passes it. Only the error gate
// sees what happened.
func TestDiffErrorThresholdCatchesAServerThatBeganFailingFast(t *testing.T) {
	working := saveRun(t, slowServer(t, 30*time.Millisecond).URL)
	broken := saveRun(t, failingServer(t).URL)

	code, stdout, _ := exec(t, "diff", "-threshold", "20", working, broken)
	if code != 0 || !strings.Contains(stdout, "ok: corrected p99") {
		t.Fatalf("-threshold alone: exit %d, want 0: the failing server is faster:\n%s", code, stdout)
	}

	code, stdout, _ = exec(t, "diff", "-error-threshold", "1", working, broken)
	if code != 1 {
		t.Fatalf("-error-threshold 1: exit %d, want 1:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "REGRESSION: error rate rose from 0.0% to 100.0% (+100.0 points), over the 1-point threshold") {
		t.Errorf("no REGRESSION line with the rates:\n%s", stdout)
	}

	// Both gates at once: each says what it found, and either failing fails the run.
	code, stdout, _ = exec(t, "diff", "-threshold", "20", "-error-threshold", "1", working, broken)
	if code != 1 || !strings.Contains(stdout, "ok: corrected p99") || !strings.Contains(stdout, "REGRESSION: error rate") {
		t.Errorf("both gates: exit %d, want 1 with the p99 ok and the error rate failing:\n%s", code, stdout)
	}
}

func TestDiffErrorThresholdPassesWhenErrorsDidNotRise(t *testing.T) {
	working := saveRun(t, okServer(t).URL)
	broken := saveRun(t, failingServer(t).URL)

	for name, args := range map[string][]string{
		"unchanged": {working, working},
		"fell":      {broken, working},
	} {
		code, stdout, stderr := exec(t, append([]string{"diff", "-error-threshold", "0"}, args...)...)
		if code != 0 || !strings.Contains(stdout, "ok: error rate") {
			t.Errorf("%s: exit %d, want 0 and an ok line:\n%s\nstderr: %s", name, code, stdout, stderr)
		}
	}
}

func TestDiffErrorThresholdRefusesToJudgeARunThatSentNothing(t *testing.T) {
	good := saveRun(t, okServer(t).URL)
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["scheduled"] = 0
	empty := filepath.Join(t.TempDir(), "empty.json")
	edited, _ := json.Marshal(doc)
	if err := os.WriteFile(empty, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := exec(t, "diff", "-error-threshold", "1", good, empty)

	if code != 2 || !strings.Contains(stderr, "sent no requests") {
		t.Errorf("exit %d, stderr %q, want 2 and a message that a run sent no requests:\n%s", code, stderr, stdout)
	}
	if strings.Contains(stdout, "ok: error rate") {
		t.Errorf("a gate that judged nothing said all is well:\n%s", stdout)
	}
}

func TestDiffErrorThresholdMustBeAFiniteNumber(t *testing.T) {
	good := saveRun(t, okServer(t).URL)
	for _, v := range []string{"NaN", "Inf", "-Inf"} {
		code, _, stderr := exec(t, "diff", "-error-threshold", v, good, good)
		if code != 2 || !strings.Contains(stderr, "-error-threshold must be a finite number") {
			t.Errorf("-error-threshold %s: exit %d, stderr %q, want 2 and a finite-number message", v, code, stderr)
		}
	}
}

// failing is an endpoint that got a bad status for every one of its n requests.
func failing(name string, n uint64) report.EndpointSummary {
	e := ep(name, n, 5, 50)
	e.BadStatus = n
	return e
}

// One endpoint that carries a fifth of the traffic fails completely. The whole
// run (the same real run on both sides here) is not what shows it.
func TestDiffErrorThresholdAlsoJudgesEachEndpointAndNamesTheOnesThatBeganFailing(t *testing.T) {
	a, b := savedPair(t,
		[]report.EndpointSummary{ep("GET /search", 100, 5, 50), ep("GET /orders/:id", 400, 5, 50), ep("GET /rare", 10, 5, 50)},
		[]report.EndpointSummary{failing("GET /search", 100), ep("GET /orders/:id", 400, 5, 50), failing("GET /rare", 10)})

	code, stdout, stderr := exec(t, "diff", "-error-threshold", "1", a, b)

	if code != 1 {
		t.Fatalf("exit %d, want 1: GET /search failed every request:\n%s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "ok: error rate went from") {
		t.Errorf("the whole run did not change, so its own line should say ok:\n%s", stdout)
	}
	verdict := stdout[strings.Index(stdout, "REGRESSION"):]
	for _, want := range []string{"REGRESSION: 1 endpoint's error rate rose more than 1 point", "GET /search", "0.0% -> 100.0%", "+100.0 points"} {
		if !strings.Contains(verdict, want) {
			t.Errorf("the verdict is missing %q:\n%s", want, verdict)
		}
	}
	if strings.Contains(verdict, "GET /rare") {
		t.Errorf("GET /rare was sent only 10 requests and must not be judged:\n%s", verdict)
	}
}

func TestDiffErrorThresholdSaysHowManyEndpointsItJudged(t *testing.T) {
	same := []report.EndpointSummary{ep("GET /search", 100, 5, 50), ep("GET /orders/:id", 400, 5, 50), ep("GET /rare", 10, 5, 50)}
	a, b := savedPair(t, same, same)

	code, stdout, _ := exec(t, "diff", "-error-threshold", "1", a, b)

	if code != 0 || !strings.Contains(stdout, "ok: no endpoint's error rate rose more than 1 point (2 judged, 1 not judged)") {
		t.Errorf("exit %d, want 0 and an ok line with the counts:\n%s", code, stdout)
	}
}

// With every endpoint too small to judge the gate still judged the whole run,
// but it must not suggest it judged the endpoints.
func TestDiffErrorThresholdSaysWhenNoEndpointCouldBeJudged(t *testing.T) {
	small := []report.EndpointSummary{ep("GET /a", 10, 5, 50), ep("GET /b", 10, 5, 50)}
	a, b := savedPair(t, small, small)

	code, stdout, _ := exec(t, "diff", "-error-threshold", "1", a, b)

	if code != 0 || !strings.Contains(stdout, "no endpoint was sent 100 requests in both runs, so none could be judged for errors") {
		t.Errorf("exit %d, want 0 and a note that no endpoint could be judged:\n%s", code, stdout)
	}
	if strings.Contains(stdout, "no endpoint's error rate rose") {
		t.Errorf("claims endpoints were judged:\n%s", stdout)
	}
}

func TestDiffErrorThresholdSaysNothingOfEndpointsWhenThereAreNone(t *testing.T) {
	run := saveRun(t, okServer(t).URL)

	_, stdout, _ := exec(t, "diff", "-error-threshold", "1", run, run)

	if strings.Contains(stdout, "endpoint") {
		t.Errorf("a plain run has no endpoints, yet the gate mentions them:\n%s", stdout)
	}
}

// withEndpoints saves a copy of the run at path with these endpoints.
func withEndpoints(t *testing.T, path string, eps []report.EndpointSummary) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := report.ReadSummary(f)
	if err != nil {
		t.Fatal(err)
	}
	s.Endpoints = eps
	out := filepath.Join(t.TempDir(), "with-endpoints.json")
	if err := writeJSONFile(out, s); err != nil {
		t.Fatal(err)
	}
	return out
}

// The whole run's error rate is the gate's first verdict and must not be lost
// when the endpoints, judged separately, are fine.
func TestDiffErrorThresholdKeepsTheWholeRunVerdictWhenTheEndpointsAreFine(t *testing.T) {
	steady := []report.EndpointSummary{ep("GET /a", 300, 5, 50), ep("GET /b", 300, 5, 50)}
	a := withEndpoints(t, saveRun(t, okServer(t).URL), steady)
	b := withEndpoints(t, saveRun(t, failingServer(t).URL), steady)

	code, stdout, _ := exec(t, "diff", "-error-threshold", "1", a, b)

	if code != 1 || !strings.Contains(stdout, "REGRESSION: error rate rose from 0.0% to 100.0%") {
		t.Errorf("exit %d, want 1 and the whole run's REGRESSION:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "ok: no endpoint's error rate rose") {
		t.Errorf("the endpoints did not change and should be reported ok:\n%s", stdout)
	}
}
