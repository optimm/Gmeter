package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimm/Gmeter/internal/report"
)

func ep(name string, n uint64, p50, p99 float64) report.EndpointSummary {
	l := report.Latency{Count: n, P50Ms: p50, P99Ms: p99, MaxMs: p99}
	return report.EndpointSummary{Endpoint: name, Scheduled: n, Corrected: l, Service: l}
}

// savedPair writes two summaries that are the same real run, so their whole-run
// numbers are identical, and gives each the endpoints it is passed.
func savedPair(t *testing.T, a, b []report.EndpointSummary) (pathA, pathB string) {
	t.Helper()
	f, err := os.Open(saveRun(t, okServer(t).URL))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	base, err := report.ReadSummary(f)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, eps []report.EndpointSummary) string {
		s := base
		s.Endpoints = eps
		path := filepath.Join(t.TempDir(), name)
		out, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		if err := s.WriteJSON(out); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return write("a.json", a), write("b.json", b)
}

var (
	endpointsA = []report.EndpointSummary{ep("GET /search", 500, 40, 50), ep("GET /orders/:id", 500, 4, 20), ep("GET /rare", 10, 4, 20)}
	endpointsB = []report.EndpointSummary{ep("GET /search", 500, 41, 150), ep("GET /orders/:id", 500, 4, 21), ep("GET /rare", 10, 4, 900)}
)

func TestDiffShowsHowEachEndpointChanged(t *testing.T) {
	a, b := savedPair(t, endpointsA, endpointsB)

	code, stdout, stderr := exec(t, "diff", a, b)

	if code != 0 {
		t.Fatalf("exit %d without a threshold, want 0; stderr: %s", code, stderr)
	}
	for _, want := range []string{"endpoints, worst p99 change first", "GET /search", "50.0ms", "150.0ms", "+200.0%", "GET /rare", "* fewer than 100"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("diff output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestDiffEndpointThresholdFailsOnAnEndpointRegressionAndNamesIt(t *testing.T) {
	a, b := savedPair(t, endpointsA, endpointsB)

	code, stdout, _ := exec(t, "diff", "-endpoint-threshold", "50", a, b)

	if code != 1 {
		t.Fatalf("exit %d, want 1: GET /search went from 50ms to 150ms:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "REGRESSION: 1 endpoint's corrected p99 rose more than 50%") {
		t.Errorf("want a REGRESSION line saying how many endpoints and by how much:\n%s", stdout)
	}
	verdict := stdout[strings.Index(stdout, "REGRESSION"):]
	if !strings.Contains(verdict, "GET /search") || !strings.Contains(verdict, "+200.0%") || !strings.Contains(verdict, "50.0ms -> 150.0ms") {
		t.Errorf("the verdict should name GET /search with its change:\n%s", verdict)
	}
	// /orders moved +5% and /rare +4400%, but had too few requests to judge.
	if strings.Contains(verdict, "GET /orders/:id") || strings.Contains(verdict, "GET /rare") {
		t.Errorf("the verdict should name only the regressed, well-sampled endpoint:\n%s", verdict)
	}
}

func TestDiffEndpointThresholdPassesAndSaysWhatItJudged(t *testing.T) {
	a, b := savedPair(t, endpointsA, endpointsB)

	code, stdout, _ := exec(t, "diff", "-endpoint-threshold", "500", a, b)

	if code != 0 {
		t.Fatalf("exit %d, want 0: nothing rose by more than 500%%:\n%s", code, stdout)
	}
	if want := "ok: no endpoint's corrected p99 rose more than 500% (2 judged, 1 not judged)"; !strings.Contains(stdout, want) {
		t.Errorf("want %q:\n%s", want, stdout)
	}
}

func TestDiffTheTwoThresholdsAreIndependent(t *testing.T) {
	a, b := savedPair(t, endpointsA, endpointsB) // the whole runs are identical; one endpoint got slower

	code, stdout, _ := exec(t, "diff", "-threshold", "20", "-endpoint-threshold", "50", a, b)

	if code != 1 || !strings.Contains(stdout, "ok: corrected p99 changed by +0.0%") || !strings.Contains(stdout, "REGRESSION: 1 endpoint's") {
		t.Errorf("exit %d, want 1: the whole run is fine and GET /search is not, and the output should say both:\n%s", code, stdout)
	}

	code, stdout, _ = exec(t, "diff", "-threshold", "20", a, b)
	if code != 0 || strings.Contains(stdout, "endpoint's") {
		t.Errorf("-threshold alone: exit %d, want 0 and no endpoint verdict:\n%s", code, stdout)
	}
}

func TestDiffEndpointThresholdWontPassWhenThereIsNothingToJudge(t *testing.T) {
	plainA, plainB := savedPair(t, nil, nil)
	oneSided, withEndpoints := savedPair(t, nil, endpointsB)
	thin := []report.EndpointSummary{ep("GET /a", 20, 1, 5), ep("GET /b", 20, 1, 5)}
	thinA, thinB := savedPair(t, thin, thin)

	for name, tc := range map[string]struct {
		a, b string
		want string
	}{
		"neither has a breakdown":     {plainA, plainB, "per-endpoint breakdown"},
		"only one has a breakdown":    {oneSided, withEndpoints, "per-endpoint breakdown"},
		"no endpoint is well-sampled": {thinA, thinB, "at least 100 requests"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := exec(t, "diff", "-endpoint-threshold", "10", tc.a, tc.b)
			if code != 2 {
				t.Errorf("exit %d, want 2: a gate that judged nothing must not pass", code)
			}
			if !strings.Contains(stderr, "-endpoint-threshold") || !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q should explain why, mentioning -endpoint-threshold and %q", stderr, tc.want)
			}
		})
	}
}

func TestDiffRefusesThresholdsThatCouldNeverFail(t *testing.T) {
	a, b := savedPair(t, endpointsA, endpointsB)
	for _, flag := range []string{"-threshold", "-endpoint-threshold"} {
		for _, value := range []string{"NaN", "+Inf"} {
			code, _, stderr := exec(t, "diff", flag, value, a, b)
			if code != 2 || !strings.Contains(stderr, flag) {
				t.Errorf("%s %s: exit %d, stderr %q, want exit 2 naming the flag: every comparison with it is false, so the gate would always pass", flag, value, code, stderr)
			}
		}
	}
}

func TestDiffEndpointVerdictLinesAreAligned(t *testing.T) {
	a, b := savedPair(t,
		[]report.EndpointSummary{ep("GET /search", 500, 40, 50), ep("GET /users/:id/cart", 500, 4, 20), ep("GET /x", 500, 4, 20)},
		[]report.EndpointSummary{ep("GET /search", 500, 41, 150), ep("GET /users/:id/cart", 500, 4, 80), ep("GET /x", 500, 4, 20)})

	_, stdout, _ := exec(t, "diff", "-endpoint-threshold", "50", a, b)

	var cols []int
	for _, line := range strings.Split(stdout[strings.Index(stdout, "REGRESSION"):], "\n") {
		if strings.HasPrefix(line, "  GET ") {
			cols = append(cols, strings.Index(line, "+"))
		}
	}
	if len(cols) != 2 || cols[0] != cols[1] {
		t.Errorf("the change should start in the same column on every line, got %v:\n%s", cols, stdout)
	}
}
