package report

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

// ep is an endpoint that was sent n requests, all with the given corrected p50
// and p99 in milliseconds.
func ep(name string, n uint64, p50, p99 float64) EndpointSummary {
	l := Latency{Count: n, P50Ms: p50, P99Ms: p99, MaxMs: p99}
	return EndpointSummary{Endpoint: name, Scheduled: n, Corrected: l, Service: l}
}

// compared returns the comparison of two otherwise identical runs that
// differ only in their endpoints.
func compared(t *testing.T, a, b []EndpointSummary) Comparison {
	t.Helper()
	sa, sb := mkSummary(100*time.Millisecond, 2), mkSummary(100*time.Millisecond, 2)
	sa.Endpoints, sb.Endpoints = a, b
	c, err := Compare(sa, sb)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func endpointNames(rows []EndpointRow) string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Endpoint
	}
	return strings.Join(names, ",")
}

func TestCompareMatchesEndpointsByNameAndMeasuresTheirChange(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{ep("GET /a", 500, 5, 50), ep("GET /b", 400, 5, 100)},
		[]EndpointSummary{ep("GET /b", 400, 6, 200), ep("GET /a", 500, 5, 55)}, // in a different order
	)

	if got := endpointNames(c.Endpoints); got != "GET /b,GET /a" {
		t.Fatalf("endpoints = %s, want GET /b,GET /a: matched by name, the biggest p99 increase first", got)
	}
	b := c.Endpoints[0]
	if b.RequestsA != 400 || b.RequestsB != 400 || b.P50A != 5 || b.P50B != 6 || b.P99A != 100 || b.P99B != 200 || b.P99ChangePct != 100 {
		t.Errorf("GET /b = %+v, want 400/400 requests, p50 5 -> 6, p99 100 -> 200 (+100%%)", b)
	}
	if a := c.Endpoints[1]; a.P99A != 50 || a.P99B != 55 || a.P99ChangePct < 9.99 || a.P99ChangePct > 10.01 {
		t.Errorf("GET /a = %+v, want p99 50 -> 55 (+10%%)", a)
	}
	if len(c.OnlyA)+len(c.OnlyB) != 0 || len(c.Notes) != 0 {
		t.Errorf("only A %v, only B %v, notes %v: want none, the runs have the same endpoints", c.OnlyA, c.OnlyB, c.Notes)
	}
}

func TestCompareMarksEndpointsWithTooFewRequestsAndRanksThemLast(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{ep("GET /busy", 500, 5, 50), ep("GET /rare", MinEndpointSamples-1, 5, 10), ep("GET /edge", MinEndpointSamples, 5, 10)},
		[]EndpointSummary{ep("GET /busy", 500, 5, 51), ep("GET /rare", 500, 5, 100), ep("GET /edge", MinEndpointSamples, 5, 20)},
	)

	// /rare changed by +900% and /edge by +100%, /busy by +2%, but a p99 from
	// fewer than 100 requests is nearly its maximum, so /rare comes last.
	if got := endpointNames(c.Endpoints); got != "GET /edge,GET /busy,GET /rare" {
		t.Fatalf("endpoints = %s, want GET /edge,GET /busy,GET /rare", got)
	}
	for _, r := range c.Endpoints {
		if want := r.Endpoint == "GET /rare"; r.Few != want {
			t.Errorf("%s: Few = %v, want %v (fewer than %d samples in A or B)", r.Endpoint, r.Few, want, MinEndpointSamples)
		}
	}
}

func TestCompareRanksEndpointsWithNoBaselineBelowThoseWithAChange(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{ep("GET /new-slow", 500, 0, 0), ep("GET /b", 500, 5, 50), ep("GET /a", 500, 5, 50)},
		[]EndpointSummary{ep("GET /new-slow", 500, 9, 90), ep("GET /b", 500, 5, 50), ep("GET /a", 500, 5, 50)},
	)

	if got := endpointNames(c.Endpoints); got != "GET /a,GET /b,GET /new-slow" {
		t.Fatalf("endpoints = %s, want GET /a,GET /b,GET /new-slow: ties by name, a missing baseline last", got)
	}
	if !math.IsNaN(c.Endpoints[2].P99ChangePct) {
		t.Errorf("change = %v, want NaN when A's p99 is 0: there is nothing to be relative to", c.Endpoints[2].P99ChangePct)
	}
}

func TestCompareListsEndpointsOnlyOneRunHas(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{ep("GET /both", 500, 5, 50), ep("GET /gone", 300, 5, 50), ep("GET /also-gone", 200, 5, 50)},
		[]EndpointSummary{ep("GET /both", 500, 5, 50), ep("GET /new", 100, 5, 50)},
	)

	if got := endpointNames(c.Endpoints); got != "GET /both" {
		t.Errorf("compared %s, want only GET /both", got)
	}
	if got := strings.Join(c.OnlyA, ","); got != "GET /gone,GET /also-gone" {
		t.Errorf("only in A = %s, want GET /gone,GET /also-gone: in A's order, busiest first", got)
	}
	if got := strings.Join(c.OnlyB, ","); got != "GET /new" {
		t.Errorf("only in B = %s, want GET /new", got)
	}
}

func TestCompareSaysWhenOnlyOneRunHasAnEndpointBreakdown(t *testing.T) {
	c := compared(t, nil, []EndpointSummary{ep("GET /a", 500, 5, 50), ep("GET /b", 500, 5, 50)})

	if len(c.Endpoints) != 0 || len(c.OnlyB) != 0 {
		t.Errorf("compared %v, only in B %v: with no breakdown in A there is nothing to compare", endpointNames(c.Endpoints), c.OnlyB)
	}
	if len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "only B") || !strings.Contains(c.Notes[0], "endpoint") {
		t.Errorf("notes = %q, want one saying that only B has an endpoint breakdown", c.Notes)
	}

	if none := compared(t, nil, nil); len(none.Notes) != 0 {
		t.Errorf("neither run has endpoints, yet notes = %q", none.Notes)
	}
}

func TestEndpointRegressionsAreTheWellSampledOnesPastTheThreshold(t *testing.T) {
	dead := EndpointSummary{Endpoint: "GET /dead", Scheduled: 500, Failed: 500} // answered before, never now
	deadBefore := ep("GET /dead", 500, 5, 50)
	c := compared(t,
		[]EndpointSummary{ep("GET /slower", 500, 5, 50), ep("GET /same", 500, 5, 50), ep("GET /faster", 500, 5, 50),
			ep("GET /doubled", 500, 5, 50), ep("GET /rare", 10, 5, 50), ep("GET /nobase", 500, 0, 0), deadBefore},
		[]EndpointSummary{ep("GET /slower", 500, 5, 70), ep("GET /same", 500, 5, 50), ep("GET /faster", 500, 5, 20),
			ep("GET /doubled", 500, 5, 100), ep("GET /rare", 10, 5, 500), ep("GET /nobase", 500, 5, 50), dead},
	)

	if got := endpointNames(c.EndpointRegressions(20)); got != "GET /doubled,GET /slower" {
		t.Errorf("regressions over 20%% = %q, want GET /doubled (+100%%), GET /slower (+40%%), worst first", got)
	}
	if got := endpointNames(c.EndpointRegressions(60)); got != "GET /doubled" {
		t.Errorf("regressions over 60%% = %q, want only GET /doubled", got)
	}
	// +100% exactly: a change equal to the threshold is not over it.
	if got := endpointNames(c.EndpointRegressions(100)); got != "" {
		t.Errorf("regressions over 100%% = %q, want none", got)
	}
	// A rare endpoint's p99 is noise, there is nothing to measure a change
	// against for /nobase, and /dead has no latency to compare: none are judged.
	if n := c.EndpointsGated(); n != 4 {
		t.Errorf("EndpointsGated = %d, want 4 (/slower, /same, /faster, /doubled)", n)
	}
}

func TestEndpointDiffTableInTheTextReport(t *testing.T) {
	searchB := ep("GET /search", 625, 43, 210)
	searchB.BadStatus, searchB.Failed = 4, 3 // 7 errors in B, none in A
	c := compared(t,
		[]EndpointSummary{ep("GET /search", 625, 42.6, 105), ep("GET /orders/:id", 1961, 4.3, 72.9), ep("GET /rare", 20, 4, 10), ep("GET /gone", 5, 1, 1)},
		[]EndpointSummary{searchB, ep("GET /orders/:id", 1961, 4.4, 73.1), ep("GET /rare", 20, 4, 50), ep("GET /new", 7, 1, 1)},
	)
	var buf bytes.Buffer
	if err := c.WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{"endpoints, worst p99 change first", "p99 change", "errors", "only in A: GET /gone (5 requests)", "only in B: GET /new (7 requests)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	rows := tableRows(out)
	// requests A and B, p50 A and B, p99 A and B, the change, errors A to B
	if got := rows["GET /search"]; got != "625 625 42.6ms 43.0ms 105.0ms 210.0ms +100.0% 0→7" {
		t.Errorf("GET /search row = %q", got)
	}
	if got := rows["GET /rare"]; got != "20 20 4.0ms 4.0ms 10.0ms 50.0ms +400.0% 0→0 *" {
		t.Errorf("GET /rare row = %q, want it marked with *", got)
	}
	if !strings.Contains(out, "* fewer than 100 requests") {
		t.Errorf("a marked row needs its footnote:\n%s", out)
	}
	if strings.Index(out, "GET /search") > strings.Index(out, "GET /orders/:id") || strings.Index(out, "GET /orders/:id") > strings.Index(out, "GET /rare") {
		t.Errorf("rows should run GET /search (+100%%), GET /orders/:id (+0.3%%), GET /rare (*):\n%s", out)
	}
}

// tableRows returns the cells after the endpoint name of each row of the
// endpoint table, by endpoint name.
func tableRows(out string) map[string]string {
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); strings.HasPrefix(line, "  GET ") && len(f) >= 10 {
			rows[f[0]+" "+f[1]] = strings.Join(f[2:], " ")
		}
	}
	return rows
}

func TestEndpointDiffTableShowsDashesWhereThereIsNoLatency(t *testing.T) {
	deadA := EndpointSummary{Endpoint: "GET /dead", Scheduled: 10, Failed: 10} // every request failed to connect: no latency samples
	deadB := EndpointSummary{Endpoint: "GET /dead", Scheduled: 10, Failed: 10}
	c := compared(t, []EndpointSummary{ep("GET /a", 500, 5, 50), deadA}, []EndpointSummary{ep("GET /a", 500, 5, 50), deadB})
	var buf bytes.Buffer
	if err := c.WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	if got := tableRows(buf.String())["GET /dead"]; got != "10 10 - - - - n/a 10\u219210 *" {
		t.Errorf("GET /dead row = %q, want dashes and n/a where nothing was measured, and its errors", got)
	}
}

func TestEndpointWithNoSamplesInOneRunHasNoMeasurableChange(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{ep("GET /a", 500, 5, 50), ep("GET /b", 500, 5, 50)},
		[]EndpointSummary{ep("GET /a", 500, 5, 50), {Endpoint: "GET /b", Scheduled: 500, Failed: 500}}, // answered before, now never
	)

	b := c.Endpoints[1]
	if b.Endpoint != "GET /b" || !math.IsNaN(b.P99ChangePct) || !b.Few {
		t.Fatalf("%+v, want GET /b last, with no change (NaN) and marked as too thin to judge", b)
	}
	if b.ErrorsA != 0 || b.ErrorsB != 500 {
		t.Errorf("errors %d -> %d, want 0 -> 500: it has to be visible that it stopped answering", b.ErrorsA, b.ErrorsB)
	}
}

func TestEndpointDiffTableIsLeftOutWhenItWouldOnlyRepeatTheTotals(t *testing.T) {
	for name, c := range map[string]Comparison{
		"no endpoints":      compared(t, nil, nil),
		"a single endpoint": compared(t, []EndpointSummary{ep("GET /a", 500, 5, 50)}, []EndpointSummary{ep("GET /a", 500, 5, 60)}),
	} {
		var buf bytes.Buffer
		if err := c.WriteText(&buf, "a.json", "b.json"); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "endpoints, worst") {
			t.Errorf("%s: the endpoint table would only repeat the run's own numbers:\n%s", name, buf.String())
		}
	}
}
