package report

import (
	"math"
	"strings"
	"testing"
)

// epPhased is an endpoint that answered n requests, each of which spent on
// average the given milliseconds queued, sending, waiting and transferring, plus
// any phases only some requests had.
func epPhased(name string, n uint64, queue, send, wait, transfer float64, extra ...PhaseMean) EndpointSummary {
	e := ep(name, n, 5, 50)
	e.Phases = append([]PhaseMean{
		{Phase: "queue", Count: n, MeanMs: queue},
		{Phase: "send", Count: n, MeanMs: send},
		{Phase: "wait", Count: n, MeanMs: wait},
		{Phase: "transfer", Count: n, MeanMs: transfer},
	}, extra...)
	return e
}

func rowNamed(t *testing.T, c Comparison, name string) EndpointRow {
	t.Helper()
	for _, r := range c.Endpoints {
		if r.Endpoint == name {
			return r
		}
	}
	t.Fatalf("no endpoint %q in %s", name, endpointNames(c.Endpoints))
	return EndpointRow{}
}

func TestCompareMeasuresWhereEachEndpointsTimeMoved(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{
			epPhased("GET /search", 500, 1, 0.1, 40, 1),
			epPhased("GET /orders", 500, 1, 0.1, 5, 1),
		},
		[]EndpointSummary{
			epPhased("GET /search", 500, 1.5, 0.1, 90, 1),
			// Half as many requests, each as before: nothing moved.
			epPhased("GET /orders", 250, 1, 0.1, 5, 1),
		})

	search := rowNamed(t, c, "GET /search")
	// queue, connect, send, wait, transfer
	want := []float64{0.5, 0, 0, 50, 0}
	if len(search.Moves) != len(want) {
		t.Fatalf("/search moves = %v, want %d phases %v", search.Moves, len(want), want)
	}
	for i := range want {
		if math.Abs(search.Moves[i]-want[i]) > 1e-9 {
			t.Errorf("/search moved %v, want %v (queue, connect, send, wait, transfer)", search.Moves, want)
			break
		}
	}
	for i, m := range rowNamed(t, c, "GET /orders").Moves {
		if m != 0 {
			t.Errorf("/orders phase %d moved %v, want 0: a time per request is of each run's own answered requests", i, m)
		}
	}
}

func TestCompareFoldsDNSConnectAndTLSIntoOneColumn(t *testing.T) {
	// A: of 100 requests, 10 looked the name up (1ms) and did TLS (4ms), 5 connected (2ms).
	a := epPhased("GET /a", 100, 1, 0.1, 8, 1,
		PhaseMean{Phase: "dns", Count: 10, MeanMs: 1},
		PhaseMean{Phase: "connect", Count: 5, MeanMs: 2},
		PhaseMean{Phase: "tls", Count: 10, MeanMs: 4})
	b := epPhased("GET /a", 100, 1, 0.1, 8, 1) // no connection at all
	c := compared(t, []EndpointSummary{a, epPhased("GET /b", 100, 1, 0.1, 8, 1)}, []EndpointSummary{b, epPhased("GET /b", 100, 1, 0.1, 8, 1)})

	// per request in A: 0.1 + 0.1 + 0.4 = 0.6ms, in B nothing.
	if got := rowNamed(t, c, "GET /a").Moves[1]; math.Abs(got-(-0.6)) > 1e-9 {
		t.Errorf("connect column moved %v, want -0.6ms: dns, connect and tls together", got)
	}
}

func TestEndpointHasNoPhaseMovesWhenARunLacksItsPhases(t *testing.T) {
	phased := epPhased("GET /a", 500, 1, 0.1, 8, 1)
	bare := ep("GET /a", 500, 5, 50) // answered, but phases were not recorded
	dead := ep("GET /dead", 10, 0, 0)
	dead.Corrected.Count, dead.Failed = 0, 10 // nothing answered, so no phases
	deadPhased := dead

	cases := map[string]Comparison{
		"only A has phases": compared(t, []EndpointSummary{phased, ep("GET /b", 500, 5, 50)}, []EndpointSummary{bare, ep("GET /b", 500, 5, 50)}),
		"only B has phases": compared(t, []EndpointSummary{bare, ep("GET /b", 500, 5, 50)}, []EndpointSummary{phased, ep("GET /b", 500, 5, 50)}),
		"nothing answered":  compared(t, []EndpointSummary{deadPhased, phased}, []EndpointSummary{deadPhased, phased}),
	}
	for name, c := range cases {
		target := "GET /a"
		if name == "nothing answered" {
			target = "GET /dead"
		}
		r := rowNamed(t, c, target)
		if r.Moves != nil {
			t.Errorf("%s: %s moves = %v, want none", name, target, r.Moves)
		}
		if phase, ms, ok := r.GrewMost(); ok {
			t.Errorf("%s: GrewMost = %s %v, want none", name, phase, ms)
		}
	}
}

func TestGrewMostNamesThePhaseThatGotSlowestAndHowMuch(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{epPhased("GET /search", 500, 1, 0.1, 40, 1), epPhased("GET /same", 500, 1, 0.1, 40, 1), epPhased("GET /faster", 500, 1, 0.1, 40, 1)},
		[]EndpointSummary{epPhased("GET /search", 500, 1.5, 0.1, 90, 1), epPhased("GET /same", 500, 1, 0.1, 40, 1), epPhased("GET /faster", 500, 1, 0.1, 30, 1)})

	phase, ms, ok := rowNamed(t, c, "GET /search").GrewMost()
	if !ok || phase != "wait" || math.Abs(ms-50) > 1e-9 {
		t.Errorf("GrewMost = %q %v %v, want wait 50", phase, ms, ok)
	}
	for _, name := range []string{"GET /same", "GET /faster"} {
		if phase, ms, ok := rowNamed(t, c, name).GrewMost(); ok {
			t.Errorf("%s: GrewMost = %q %v, want none: no phase got slower", name, phase, ms)
		}
	}
}

// movedRows returns the changes in each row of the table of where endpoints'
// time moved, by endpoint name.
func movedRows(out string) (rows map[string]string, section string) {
	rows = map[string]string{}
	_, section, found := strings.Cut(out, "where the time moved")
	if !found {
		return rows, ""
	}
	for _, line := range strings.Split(section, "\n")[1:] {
		f := strings.Fields(line)
		if line == "" {
			break
		}
		mark := ""
		if len(f) > 0 && f[len(f)-1] == "*" { // a few-requests mark trails the row
			f, mark = f[:len(f)-1], " *"
		}
		if strings.HasPrefix(line, "  ") && len(f) >= 7 && f[0] != "endpoint" {
			rows[strings.Join(f[:len(f)-5], " ")+mark] = strings.Join(f[len(f)-5:], " ")
		}
	}
	return rows, section
}

func TestEndpointPhaseMovesInTheTextReport(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{epPhased("GET /search", 500, 1, 0.1, 40, 1), epPhased("GET /orders", 500, 1, 0.1, 5, 1), epPhased("GET /rare", 20, 1, 0.1, 5, 1)},
		[]EndpointSummary{epPhased("GET /search", 500, 1.5, 0.1, 90, 1), epPhased("GET /orders", 500, 1, 0.1, 5, 1), epPhased("GET /rare", 20, 1, 0.1, 9, 1)})
	var buf strings.Builder
	if err := c.WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	rows, section := movedRows(out)
	// queue, connect, send, wait, transfer
	if got := rows["GET /search"]; got != "+500µs 0ns 0ns +50.0ms 0ns" {
		t.Errorf("/search row = %q", got)
	}
	if got := rows["GET /orders"]; got != "0ns 0ns 0ns 0ns 0ns" {
		t.Errorf("/orders row = %q", got)
	}
	// An endpoint with few requests is marked, as in the table above it.
	if got := rows["GET /rare *"]; got != "0ns 0ns 0ns +4.0ms 0ns" {
		t.Errorf("/rare row = %q, want it marked with *: %v", got, rows)
	}
	if !strings.Contains(section, "B minus A") || !strings.Contains(section, "dns + connect + tls") {
		t.Errorf("the table does not say what its numbers or its connect column are:\n%s", section)
	}
}

func TestEndpointPhaseMovesTableIsLeftOutWithoutPhases(t *testing.T) {
	c := compared(t,
		[]EndpointSummary{ep("GET /a", 500, 5, 50), ep("GET /b", 500, 5, 50)},
		[]EndpointSummary{ep("GET /a", 500, 5, 60), ep("GET /b", 500, 5, 50)})
	var buf strings.Builder
	if err := c.WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "where the time moved") {
		t.Errorf("a table of phase moves although no endpoint has phases:\n%s", buf.String())
	}
}

func TestEndpointPhaseMovesAreOnlyForTheEndpointsBothRunsHave(t *testing.T) {
	// An endpoint only one run has is listed above the table, not compared in it.
	c := compared(t,
		[]EndpointSummary{epPhased("GET /a", 500, 1, 0.1, 8, 1), epPhased("GET /gone", 500, 1, 0.1, 8, 1)},
		[]EndpointSummary{epPhased("GET /a", 500, 1, 0.1, 8, 1), epPhased("GET /new", 500, 1, 0.1, 8, 1)})
	var buf strings.Builder
	if err := c.WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	rows, _ := movedRows(buf.String())
	if len(rows) != 1 || rows["GET /a"] == "" {
		t.Errorf("rows = %v, want just GET /a, the only endpoint in both", rows)
	}
}
