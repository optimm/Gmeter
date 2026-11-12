package report

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// totals is a label's phases for n responses, in runner.PhaseNames order. Every
// response had the queue, send, wait and transfer given; dns, connect and tls
// are given as (requests that had it, each one's time).
type connPhase struct {
	n    uint64
	each time.Duration
}

func totals(n uint64, queue, send, wait, transfer time.Duration, dns, connect, tls connPhase) []runner.PhaseTotal {
	all := func(d time.Duration) runner.PhaseTotal {
		return runner.PhaseTotal{Requests: n, Total: time.Duration(n) * d}
	}
	some := func(c connPhase) runner.PhaseTotal {
		return runner.PhaseTotal{Requests: c.n, Total: time.Duration(c.n) * c.each}
	}
	out := []runner.PhaseTotal{all(queue), some(dns), some(connect), some(tls), all(send), all(wait), all(transfer)}
	for i := range out {
		out[i].Phase = runner.PhaseNames[i]
	}
	return out
}

// phasedEndpoints is two endpoints with phases and one that was never answered.
//
//	GET /a: 100 responses of 1ms queue, 0.1ms send, 8ms wait, 1ms transfer; 5 of them connected, 4ms each
//	GET /b: 10 responses of 20ms wait; every one looked up the name (1ms), connected (2ms) and did TLS (4ms)
//	GET /c: nothing answered
func phasedEndpoints() *runner.Result {
	res := sampleResult()
	a := labelResult("GET /a", 100, 0, 0, 100, 10*time.Millisecond, 10*time.Millisecond)
	a.Phases = totals(100, time.Millisecond, 100*time.Microsecond, 8*time.Millisecond, time.Millisecond,
		connPhase{}, connPhase{5, 4 * time.Millisecond}, connPhase{})
	b := labelResult("GET /b", 10, 0, 0, 10, 27*time.Millisecond, 27*time.Millisecond)
	b.Phases = totals(10, 0, 0, 20*time.Millisecond, 0,
		connPhase{10, time.Millisecond}, connPhase{10, 2 * time.Millisecond}, connPhase{10, 4 * time.Millisecond})
	c := labelResult("GET /c", 3, 3, 0, 0, 0, 0)
	c.Phases = totals(0, 0, 0, 0, 0, connPhase{}, connPhase{}, connPhase{})
	res.Labels = []runner.LabelResult{a, b, c}
	return res
}

func TestSummarizeGivesEachEndpointItsPhaseMeans(t *testing.T) {
	s := Summarize(phasedEndpoints(), 10)

	a := s.Endpoints[0]
	var names []string
	var shares float64
	by := map[string]PhaseMean{}
	for _, p := range a.Phases {
		names = append(names, p.Phase)
		shares += p.SharePct
		by[p.Phase] = p
	}
	// dns and tls had no requests: a phase nobody went through is not a row.
	if got := strings.Join(names, ","); got != "queue,connect,send,wait,transfer" {
		t.Fatalf("GET /a phases = %s, want queue,connect,send,wait,transfer", got)
	}
	if math.Abs(shares-100) > 1e-9 {
		t.Errorf("GET /a shares add up to %v, want 100", shares)
	}
	// 100ms queue + 20ms connect + 10ms send + 800ms wait + 100ms transfer = 1030ms
	if w := by["wait"]; w.Count != 100 || w.MeanMs != 8 || math.Abs(w.SharePct-800.0/1030*100) > 1e-9 {
		t.Errorf("wait = %+v, want 100 requests averaging 8ms and %.4g%% of the endpoint's time", w, 800.0/1030*100)
	}
	// The mean is of the requests that connected, the share of all the time.
	if c := by["connect"]; c.Count != 5 || c.MeanMs != 4 || math.Abs(c.SharePct-20.0/1030*100) > 1e-9 {
		t.Errorf("connect = %+v, want 5 requests averaging 4ms and %.4g%% of the endpoint's time", c, 20.0/1030*100)
	}
	if len(s.Endpoints[2].Phases) != 0 {
		t.Errorf("GET /c has phases %v, want none: nothing was answered", s.Endpoints[2].Phases)
	}
}

func TestEndpointPhasesAreLeftOutOfTheJSONWhenNotRecorded(t *testing.T) {
	s := Summarize(endpointResult(), 10) // endpoints, but no phases
	for _, e := range s.Endpoints {
		if e.Phases != nil {
			t.Errorf("%s has phases %v, want none", e.Endpoint, e.Phases)
		}
	}
	var buf bytes.Buffer
	if err := s.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"phases"`) {
		t.Errorf("the JSON has a phases key although none were recorded:\n%s", buf.String())
	}
}

func TestEndpointPhasesSurviveTheJSON(t *testing.T) {
	s := Summarize(phasedEndpoints(), 10)
	var buf bytes.Buffer
	if err := s.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Summary
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Endpoints[0].Phases) != 5 || back.Endpoints[0].Phases[3] != s.Endpoints[0].Phases[3] {
		t.Errorf("after a round trip GET /a has %+v, want %+v", back.Endpoints[0].Phases, s.Endpoints[0].Phases)
	}
}

// phaseRows returns the numbers of each row of the endpoint phase table
// (answered, mean, queue, connect, send, wait, transfer) by endpoint name.
func phaseRows(t *testing.T, out string) map[string][]string {
	t.Helper()
	_, section, found := strings.Cut(out, "endpoints, where the time goes")
	if !found {
		t.Fatalf("no endpoint phase table in:\n%s", out)
	}
	rows := map[string][]string{}
	for i, line := range strings.Split(section, "\n")[1:] {
		f := strings.Fields(line)
		if line == "" {
			break
		}
		if i == 0 { // the header
			continue
		}
		if len(f) >= 8 && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			rows[strings.Join(f[:len(f)-7], " ")] = f[len(f)-7:]
		}
	}
	return rows
}

func TestEndpointPhaseTableShowsTheMeanTimePerAnsweredRequest(t *testing.T) {
	var buf strings.Builder
	if err := Summarize(phasedEndpoints(), 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	rows := phaseRows(t, out)

	// GET /a: per answered request 1ms queue, 0.2ms connect (20ms over 100
	// requests, not 4ms), 0.1ms send, 8ms wait, 1ms transfer: 10.3ms in all.
	want := map[string]string{
		"GET /a": "100 10.3ms 1.0ms 200µs 100µs 8.0ms 1.0ms",
		// GET /b: dns, connect and tls are one column: 1+2+4 = 7ms.
		"GET /b": "10 27.0ms 0ns 7.0ms 0ns 20.0ms 0ns",
	}
	if len(rows) != len(want) {
		t.Errorf("rows for %d endpoints, want %d (GET /c was never answered): %v", len(rows), len(want), rows)
	}
	for name, w := range want {
		if got := strings.Join(rows[name], " "); got != w {
			t.Errorf("%s: %q, want %q", name, got, w)
		}
	}
	if !strings.Contains(out, "dns + connect + tls") {
		t.Errorf("the table does not say what its connect column is:\n%s", out)
	}
}

func TestEndpointPhaseTableIsLeftOutWithoutPhasesOrWithOneEndpoint(t *testing.T) {
	one := phasedEndpoints()
	one.Labels = one.Labels[:1]
	for name, res := range map[string]*runner.Result{
		"endpoints without phases": endpointResult(),
		"one endpoint":             one,
	} {
		var buf strings.Builder
		if err := Summarize(res, 10).WriteText(&buf); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "endpoints, where the time goes") {
			t.Errorf("%s: the report has an endpoint phase table:\n%s", name, buf.String())
		}
	}
}
