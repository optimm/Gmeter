package report

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
)

// waits is a wait distribution of n samples of d each, and tails of tail.
func waits(n int, d time.Duration, tails int, tail time.Duration) *hist.Histogram {
	h := hist.New()
	for range n {
		h.Record(d)
	}
	for range tails {
		h.Record(tail)
	}
	return h
}

func TestSummarizeGivesEachEndpointItsWaitDistribution(t *testing.T) {
	res := phasedEndpoints()
	res.Labels[0].Wait = waits(98, 8*time.Millisecond, 2, 200*time.Millisecond) // GET /a
	res.Labels[1].Wait = waits(9, 20*time.Millisecond, 1, 20*time.Millisecond)  // GET /b
	res.Labels[2].Wait = hist.New()                                             // GET /c: nothing answered
	s := Summarize(res, 10)

	a := s.Endpoints[0].Wait
	if a == nil {
		t.Fatal("GET /a has no wait distribution")
	}
	if a.Count != 100 || math.Abs(a.P50Ms-8) > 0.01 || math.Abs(a.MaxMs-200) > 0.2 {
		t.Errorf("GET /a wait = %+v, want 100 samples, p50 8ms, max 200ms", *a)
	}
	if s.Endpoints[2].Wait != nil {
		t.Errorf("GET /c wait = %+v, want none: nothing was answered", *s.Endpoints[2].Wait)
	}

	var buf bytes.Buffer
	if err := s.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), `"latency_wait"`) != 2 {
		t.Errorf("want latency_wait for the two answered endpoints in the JSON:\n%s", buf.String())
	}
}

func TestEndpointsWithoutPhasesHaveNoWaitInTheJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(endpointResult(), 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"latency_wait"`) {
		t.Errorf("latency_wait in the JSON although phases were not recorded:\n%s", buf.String())
	}
}

func TestDiffComparesEachEndpointsWaitP99(t *testing.T) {
	withWait := func(e EndpointSummary, p99 float64) EndpointSummary {
		e.Wait = &Latency{Count: e.Corrected.Count, P50Ms: p99 / 2, P99Ms: p99, MaxMs: p99}
		return e
	}
	c := compared(t,
		[]EndpointSummary{withWait(ep("GET /a", 500, 5, 50), 30), ep("GET /b", 500, 5, 50), withWait(ep("GET /c", 500, 5, 50), 30)},
		[]EndpointSummary{withWait(ep("GET /a", 500, 5, 90), 70), withWait(ep("GET /b", 500, 5, 50), 40), ep("GET /c", 500, 5, 50)})

	a, b, ok := rowNamed(t, c, "GET /a").WaitP99s()
	if !ok || a != 30 || b != 70 {
		t.Errorf("GET /a wait p99 = %v -> %v (%v), want 30 -> 70", a, b, ok)
	}
	for _, name := range []string{"GET /b", "GET /c"} {
		if _, _, ok := rowNamed(t, c, name).WaitP99s(); ok {
			t.Errorf("%s has a wait p99 in only one run, but WaitP99s says both", name)
		}
	}
}

func TestEndpointPhaseTableShowsEachEndpointsWaitP99(t *testing.T) {
	res := phasedEndpoints()
	res.Labels[0].Wait = waits(98, 8*time.Millisecond, 2, 200*time.Millisecond) // GET /a: a mean of 11.8ms hides a 200ms tail
	res.Labels[1].Wait = waits(9, 20*time.Millisecond, 1, 20*time.Millisecond)
	var buf strings.Builder
	if err := Summarize(res, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	rows := phaseRows(t, buf.String())
	if got := rows["GET /a"][7]; got != "200.0ms" {
		t.Errorf("GET /a wait p99 = %q, want 200.0ms: %v", got, rows)
	}
	if got := rows["GET /b"][7]; got != "20.0ms" {
		t.Errorf("GET /b wait p99 = %q, want 20.0ms", got)
	}
	if !strings.Contains(buf.String(), "wait p99") {
		t.Errorf("the table does not name its wait p99 column:\n%s", buf.String())
	}
}
