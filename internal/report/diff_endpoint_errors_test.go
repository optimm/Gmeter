package report

import (
	"math"
	"testing"
)

// epErr is an endpoint that was sent n requests, failed of which failed and bad
// got a bad status.
func epErr(name string, n, failed, bad uint64) EndpointSummary {
	e := ep(name, n, 5, 50)
	e.Failed, e.BadStatus = failed, bad
	return e
}

func TestEndpointErrorPercentsAreOfTheRequestsSentToTheEndpoint(t *testing.T) {
	// A: 4 of 200, 2.0%. B: 60 of 400, 15.0%.
	c := compared(t,
		[]EndpointSummary{epErr("GET /a", 200, 3, 1), epErr("GET /b", 200, 0, 0)},
		[]EndpointSummary{epErr("GET /a", 400, 50, 10), epErr("GET /b", 200, 0, 0)})

	a, b, ok := rowNamed(t, c, "GET /a").ErrorPercents()
	if !ok || math.Abs(a-2) > 1e-9 || math.Abs(b-15) > 1e-9 {
		t.Errorf("ErrorPercents = %v, %v, %v, want 2, 15, true", a, b, ok)
	}
}

func TestEndpointErrorRegressionsListTheBiggestRiseFirst(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{epErr("GET /a-small", 200, 0, 0), epErr("GET /b-big", 200, 0, 0), epErr("GET /fell", 200, 100, 0), epErr("GET /same", 200, 2, 0)},
		[]EndpointSummary{epErr("GET /a-small", 200, 24, 0), epErr("GET /b-big", 200, 100, 0), epErr("GET /fell", 200, 0, 0), epErr("GET /same", 200, 2, 0)})
	// rises: /a-small +12 points, /b-big +50 (named so that the name order is the wrong one), /fell -50, /same 0

	var got []string
	for _, r := range c.EndpointErrorRegressions(5) {
		got = append(got, r.Endpoint)
	}
	if len(got) != 2 || got[0] != "GET /b-big" || got[1] != "GET /a-small" {
		t.Errorf("regressions over 5 points = %v, want [GET /b-big GET /a-small]", got)
	}
	if n := len(c.EndpointErrorRegressions(12)); n != 1 {
		t.Errorf("%d regressions over 12 points, want 1: a rise of exactly 12 is not more than 12", n)
	}
	if n := len(c.EndpointErrorRegressions(0)); n != 2 {
		t.Errorf("%d regressions over 0 points, want 2: only a rise counts, not no change or a fall", n)
	}
}

func TestEndpointsWithFewRequestsAreNotJudgedForErrors(t *testing.T) {
	down := epErr("GET /down", 300, 300, 0) // every request failed to get a response
	down.Corrected.Count = 0
	c := compared(t,
		[]EndpointSummary{epErr("GET /down", 300, 0, 0), epErr("GET /rare", 99, 0, 0), epErr("GET /rare2", 300, 0, 0), epErr("GET /ok", 300, 0, 0)},
		[]EndpointSummary{down, epErr("GET /rare", 400, 400, 0), epErr("GET /rare2", 99, 99, 0), epErr("GET /ok", 300, 0, 0)})

	var got []string
	for _, r := range c.EndpointErrorRegressions(1) {
		got = append(got, r.Endpoint)
	}
	// /down has no latency samples at all in B, which the p99 gate cannot judge,
	// and is the worst case. /rare and /rare2 were sent 99 requests in one run.
	if len(got) != 1 || got[0] != "GET /down" {
		t.Errorf("regressions = %v, want only GET /down", got)
	}
	if n := c.EndpointsErrorGated(); n != 2 {
		t.Errorf("EndpointsErrorGated = %d, want 2 (/down and /ok)", n)
	}
	if _, _, ok := rowNamed(t, c, "GET /rare").ErrorPercents(); ok {
		t.Error("an endpoint sent 99 requests in A has an error rate")
	}
}
