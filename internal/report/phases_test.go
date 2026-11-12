package report

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/runner"
)

// phased is a run whose 100 responses spent, each, 1ms queued, 0.1ms sending,
// 8ms waiting and 1ms transferring, and 5 of them also 3ms connecting.
func phased() *runner.Result {
	res := sampleResult()
	phase := func(name string, n int, each time.Duration) runner.PhaseResult {
		h := hist.New()
		for range n {
			h.Record(each)
		}
		return runner.PhaseResult{Phase: name, Samples: h}
	}
	res.Phases = []runner.PhaseResult{
		phase("queue", 100, time.Millisecond),
		phase("dns", 0, 0),
		phase("connect", 5, 3*time.Millisecond),
		phase("tls", 0, 0),
		phase("send", 100, 100*time.Microsecond),
		phase("wait", 100, 8*time.Millisecond),
		phase("transfer", 100, time.Millisecond),
	}
	return res
}

func TestSummarizeSplitsTheLatencyIntoPhasesAndTheirShares(t *testing.T) {
	s := Summarize(phased(), 10)

	var names []string
	var shares float64
	for _, p := range s.Phases {
		names = append(names, p.Phase)
		shares += p.SharePct
	}
	// dns and tls had no samples: a phase nobody went through is not a row.
	if got := strings.Join(names, ","); got != "queue,connect,send,wait,transfer" {
		t.Fatalf("phases = %s, want queue,connect,send,wait,transfer in the order a request goes through them", got)
	}
	if math.Abs(shares-100) > 1e-9 {
		t.Errorf("shares add up to %v, want 100", shares)
	}
	// 100ms queue + 15ms connect + 10ms send + 800ms wait + 100ms transfer = 1025ms
	by := map[string]PhaseSummary{}
	for _, p := range s.Phases {
		by[p.Phase] = p
	}
	if w := by["wait"]; w.Count != 100 || w.P50Ms != 8 || w.MaxMs != 8 || math.Abs(w.SharePct-800.0/1025*100) > 1e-9 {
		t.Errorf("wait = %+v, want 100 samples of 8ms and %.4g%% of the time", w, 800.0/1025*100)
	}
	if c := by["connect"]; c.Count != 5 || c.P99Ms != 3 || math.Abs(c.SharePct-15.0/1025*100) > 1e-9 {
		t.Errorf("connect = %+v, want 5 samples of 3ms and a share of the total time (not of the connections)", c)
	}
}

func TestRunsWithoutPhasesHaveNoPhaseSection(t *testing.T) {
	empty := sampleResult()
	allEmpty := sampleResult()
	allEmpty.Phases = []runner.PhaseResult{{Phase: "queue", Samples: hist.New()}, {Phase: "wait", Samples: hist.New()}}

	for name, res := range map[string]*runner.Result{"not asked for": empty, "nothing got a response": allEmpty} {
		s := Summarize(res, 10)
		var text, js bytes.Buffer
		if err := s.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteJSON(&js); err != nil {
			t.Fatal(err)
		}
		if len(s.Phases) != 0 || strings.Contains(text.String(), "where the time goes") || strings.Contains(js.String(), `"phases"`) {
			t.Errorf("%s: want no phases anywhere, got %v\n%s\n%s", name, s.Phases, text.String(), js.String())
		}
	}
}

func TestJSONHasThePhasesAndTheyReadBack(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(phased(), 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Phases []struct {
			Phase    string  `json:"phase"`
			Count    uint64  `json:"count"`
			P99Ms    float64 `json:"p99_ms"`
			SharePct float64 `json:"share_pct"`
		} `json:"phases"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Phases) != 5 || raw.Phases[3].Phase != "wait" || raw.Phases[3].Count != 100 || raw.Phases[3].P99Ms != 8 {
		t.Errorf("phases in the JSON = %+v, want 5 with wait fourth: 100 samples, p99 8ms", raw.Phases)
	}
	back, err := ReadSummary(&buf)
	if err != nil || len(back.Phases) != 5 {
		t.Errorf("ReadSummary: %d phases, err %v, want the 5 that were written", len(back.Phases), err)
	}
}

func TestTextReportShowsWhereTheTimeGoes(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(phased(), 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "where the time goes") {
		t.Fatalf("no phase table:\n%s", out)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 6 && strings.HasPrefix(line, "  ") {
			rows[f[0]] = strings.Join(f[1:], " ")
		}
	}
	// requests, p50, p99, max, share
	for phase, want := range map[string]string{
		"queue":    "100 1.0ms 1.0ms 1.0ms 9.8%",
		"connect":  "5 3.0ms 3.0ms 3.0ms 1.5%",
		"send":     "100 100µs 100µs 100µs 1.0%",
		"wait":     "100 8.0ms 8.0ms 8.0ms 78.0%",
		"transfer": "100 1.0ms 1.0ms 1.0ms 9.8%",
	} {
		if rows[phase] != want {
			t.Errorf("%s row = %q, want %q", phase, rows[phase], want)
		}
	}
	for _, absent := range []string{"dns", "tls"} {
		if _, ok := rows[absent]; ok {
			t.Errorf("%s has no samples and should not be a row", absent)
		}
	}
	if strings.Index(out, "where the time goes") < strings.Index(out, "latency, service") {
		t.Errorf("the phases split the latency, so they belong after it:\n%s", out)
	}
	for _, gloss := range []string{"queue", "send", "wait", "transfer", "only count requests that opened a connection"} {
		if !strings.Contains(out[strings.Index(out, "where the time goes"):], gloss) {
			t.Errorf("the phase table does not explain %q:\n%s", gloss, out)
		}
	}
}

func TestPhaseTableOnlyExplainsConnectionPhasesWhenItHasThem(t *testing.T) {
	res := phased()
	res.Phases[2].Samples = hist.New() // no connect samples: every request reused a connection
	var buf bytes.Buffer
	if err := Summarize(res, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "opened a connection") {
		t.Errorf("no request opened a connection, so there is nothing to explain:\n%s", buf.String())
	}
}
