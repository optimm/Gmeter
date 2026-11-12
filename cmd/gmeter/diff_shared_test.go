package main

import (
	"strings"
	"testing"

	"github.com/optimm/Gmeter/internal/report"
)

func fourEndpoints(p99 float64, risen int, factor float64) []report.EndpointSummary {
	out := []report.EndpointSummary{
		ep("GET /a", 500, 5, p99), ep("GET /b", 500, 5, p99), ep("GET /c", 500, 5, p99), ep("GET /d", 500, 5, p99),
	}
	for i := range out[:risen] {
		out[i].Corrected.P99Ms *= factor
	}
	return out
}

// With a server that is saturated everything gets slower together, and a gate
// that only lists the endpoints suggests four problems to chase, not one.
func TestEndpointGateSaysWhenMostEndpointsRegressedTogether(t *testing.T) {
	a, b := savedPair(t, fourEndpoints(10, 0, 1), fourEndpoints(10, 3, 5))

	code, stdout, stderr := exec(t, "diff", "-endpoint-threshold", "30", a, b)

	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s\n%s", code, stderr, stdout)
	}
	verdict := stdout[strings.Index(stdout, "REGRESSION"):]
	for _, want := range []string{"REGRESSION: 3 endpoints' corrected p99 rose more than 30%", "3 of 4 judged endpoints' p99 rose by more than 50% at once", "one shared cause", "3 separate"} {
		if !strings.Contains(verdict, want) {
			t.Errorf("the verdict is missing %q:\n%s", want, verdict)
		}
	}
}

func TestEndpointGateSaysNothingOfASharedCauseWhenOneEndpointRegressed(t *testing.T) {
	a, b := savedPair(t, fourEndpoints(10, 0, 1), fourEndpoints(10, 1, 5))

	code, stdout, _ := exec(t, "diff", "-endpoint-threshold", "30", a, b)

	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, stdout)
	}
	verdict := stdout[strings.Index(stdout, "REGRESSION"):]
	if strings.Contains(verdict, "shared cause") {
		t.Errorf("one endpoint regressed, yet the verdict talks of a shared cause:\n%s", verdict)
	}
}
