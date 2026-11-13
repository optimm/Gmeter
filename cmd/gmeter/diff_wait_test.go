package main

import (
	"strings"
	"testing"

	"github.com/optimm/Gmeter/internal/report"
)

func TestARegressedEndpointShowsHowItsWaitP99Moved(t *testing.T) {
	withWait := func(e report.EndpointSummary, p99 float64) report.EndpointSummary {
		e.Wait = &report.Latency{Count: e.Corrected.Count, P50Ms: p99 / 2, P99Ms: p99, MaxMs: p99}
		return e
	}
	a, b := savedPair(t,
		[]report.EndpointSummary{withWait(ep("GET /search", 500, 40, 50), 45), ep("GET /orders/:id", 500, 4, 20)},
		[]report.EndpointSummary{withWait(ep("GET /search", 500, 41, 150), 140), ep("GET /orders/:id", 500, 4, 80)})

	code, stdout, _ := exec(t, "diff", "-endpoint-threshold", "50", a, b)

	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, stdout)
	}
	_, regs, _ := strings.Cut(stdout, "REGRESSION")
	var search, orders string
	for _, line := range strings.Split(regs, "\n") {
		switch {
		case strings.Contains(line, "GET /search"):
			search = line
		case strings.Contains(line, "GET /orders/:id"):
			orders = line
		}
	}
	if !strings.Contains(search, "wait p99 45.0ms -> 140.0ms") {
		t.Errorf("the /search line does not show its wait p99 moving: %q", search)
	}
	if orders == "" || strings.Contains(orders, "wait p99") {
		t.Errorf("the /orders line has no wait p99 in either run, so it should show none: %q", orders)
	}
}
