package main

import (
	"strings"
	"testing"

	"github.com/optimm/Gmeter/internal/report"
)

// epPhased is an endpoint that answered n requests, each of which spent on
// average the given milliseconds queued and waiting (and nothing in the others).
func epPhased(name string, n uint64, p50, p99, queue, wait float64) report.EndpointSummary {
	e := ep(name, n, p50, p99)
	e.Phases = []report.PhaseMean{
		{Phase: "queue", Count: n, MeanMs: queue},
		{Phase: "send", Count: n, MeanMs: 0.1},
		{Phase: "wait", Count: n, MeanMs: wait},
		{Phase: "transfer", Count: n, MeanMs: 1},
	}
	return e
}

var (
	phasedA = []report.EndpointSummary{epPhased("GET /search", 500, 40, 50, 1, 40), epPhased("GET /orders/:id", 500, 4, 20, 1, 4)}
	phasedB = []report.EndpointSummary{epPhased("GET /search", 500, 41, 150, 1.5, 140), epPhased("GET /orders/:id", 500, 4, 21, 1, 4)}
)

func TestDiffEndpointVerdictNamesThePhaseThatGotSlowest(t *testing.T) {
	a, b := savedPair(t, phasedA, phasedB)

	code, stdout, _ := exec(t, "diff", "-endpoint-threshold", "50", a, b)

	if code != 1 {
		t.Fatalf("exit %d, want 1: GET /search went from 50ms to 150ms:\n%s", code, stdout)
	}
	verdict := stdout[strings.Index(stdout, "REGRESSION"):]
	if !strings.Contains(verdict, "GET /search  +200.0%  (50.0ms -> 150.0ms)") {
		t.Errorf("the verdict line should keep its shape:\n%s", verdict)
	}
	if !strings.Contains(verdict, "mean grew most in wait (+100.0ms)") {
		t.Errorf("the verdict should say which phase got slowest:\n%s", verdict)
	}
}

func TestDiffEndpointVerdictSaysNothingOfPhasesThatWereNotRecorded(t *testing.T) {
	a, b := savedPair(t, endpointsA, endpointsB)

	_, stdout, _ := exec(t, "diff", "-endpoint-threshold", "50", a, b)

	if verdict := stdout[strings.Index(stdout, "REGRESSION"):]; strings.Contains(verdict, "grew most") {
		t.Errorf("the verdict names a phase although none were recorded:\n%s", verdict)
	}
}

// savedPair gives both files the run's real phases, as a plain run saves them.
func TestDiffShowsWherePhasesMovedFromRealSavedRuns(t *testing.T) {
	a, b := savedPair(t, phasedA, phasedB)

	code, stdout, stderr := exec(t, "diff", a, b)

	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, stderr)
	}
	for _, want := range []string{
		"where the time goes, per answered request", // the whole run's phases, read back from the files
		"no phase moved by as much as 10% of A's mean",
		"endpoints, where the time moved",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("diff output is missing %q:\n%s", want, stdout)
		}
	}
	_, table, _ := strings.Cut(stdout, "endpoints, where the time moved")
	if !strings.Contains(table, "GET /search") || !strings.Contains(table, "+100.0ms") {
		t.Errorf("the table of moves should show GET /search's wait up 100ms:\n%s", table)
	}
}
