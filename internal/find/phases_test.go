package find

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// stagePhases is a stage's phase totals for n answered requests that each spent
// the given milliseconds queued, connecting, sending, waiting and transferring;
// connect is counted only for the conns requests that opened a connection.
func stagePhases(n, conns uint64, queue, connect, send, wait, transfer float64) []runner.PhaseTotal {
	ms := func(per float64, count uint64) time.Duration {
		return time.Duration(per * float64(count) * float64(time.Millisecond))
	}
	return []runner.PhaseTotal{
		{Phase: "queue", Requests: n, Total: ms(queue, n)},
		// The connection's time is split over its three phases.
		{Phase: "dns", Requests: conns, Total: ms(connect/4, conns)},
		{Phase: "connect", Requests: conns, Total: ms(connect/2, conns)},
		{Phase: "tls", Requests: conns, Total: ms(connect/4, conns)},
		{Phase: "send", Requests: n, Total: ms(send, n)},
		{Phase: "wait", Requests: n, Total: ms(wait, n)},
		{Phase: "transfer", Requests: n, Total: ms(transfer, n)},
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestEvaluateKeepsWhereTheMeasuredStagesTimeWent(t *testing.T) {
	st, res := trial(1000, 10*time.Millisecond, 0, 0, 0)
	// 10 of the 1000 requests opened a connection that took 2ms: 0.02ms a request.
	st.Phases = stagePhases(1000, 10, 1, 2, 0.1, 8, 0.5)

	_, o := Evaluate(slo99, 100, st, res)
	if o.Phases == nil {
		t.Fatal("no phases, although the stage has them")
	}
	want := TrialPhases{QueueMs: 1, ConnectMs: 0.02, SendMs: 0.1, WaitMs: 8, TransferMs: 0.5}
	got := *o.Phases
	if !near(got.QueueMs, want.QueueMs) || !near(got.ConnectMs, want.ConnectMs) || !near(got.SendMs, want.SendMs) ||
		!near(got.WaitMs, want.WaitMs) || !near(got.TransferMs, want.TransferMs) {
		t.Errorf("phases = %+v, want %+v (the mean per answered request)", got, want)
	}
}

func TestEvaluateHasNoPhasesWithoutThem(t *testing.T) {
	st, res := trial(1000, 10*time.Millisecond, 0, 0, 0)
	if _, o := Evaluate(slo99, 100, st, res); o.Phases != nil {
		t.Errorf("phases = %+v, want none: the stage recorded none", o.Phases)
	}
	st.Phases = stagePhases(0, 0, 0, 0, 0, 0, 0) // recorded, but nothing answered
	if _, o := Evaluate(slo99, 100, st, res); o.Phases != nil {
		t.Errorf("phases = %+v, want none: no request was answered", o.Phases)
	}
}

// phased is found() with phases on the trials: at 750 rps queue, wait and
// connect are as given; everything else is as at 700.
func phased(queue750, wait750, connect750 float64) Outcome {
	o := found()
	for i, tr := range o.Trials {
		ob := tr.Detail.(Observation)
		p := &TrialPhases{QueueMs: 0.1, SendMs: 0.05, WaitMs: 5, TransferMs: 0.2}
		if tr.Phase == Confirm { // only the trial that made 700 the answer counts
			p = &TrialPhases{QueueMs: 0.1, SendMs: 0.05, WaitMs: 9, TransferMs: 0.2}
		}
		if tr.Rate == 750 {
			p = &TrialPhases{QueueMs: queue750, ConnectMs: connect750, SendMs: 0.05, WaitMs: wait750, TransferMs: 0.2}
		}
		ob.Phases = p
		o.Trials[i].Detail = ob
	}
	return o
}

func TestTheJSONHasEachTrialsPhases(t *testing.T) {
	var buf bytes.Buffer
	if err := NewSummary(phased(40, 20, 10), info, slo99, false).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Trials []struct {
			RPS    float64          `json:"rps"`
			Phases *json.RawMessage `json:"phases"`
		} `json:"trials"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, tr := range got.Trials {
		if tr.Phases == nil {
			t.Fatalf("trial at %g rps has no phases:\n%s", tr.RPS, buf.String())
		}
	}
	if !strings.Contains(buf.String(), `"wait_ms": 20`) {
		t.Errorf("the 750 rps trial's wait is missing:\n%s", buf.String())
	}

	buf.Reset()
	if err := NewSummary(found(), info, slo99, false).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"phases"`) {
		t.Errorf("phases in the JSON of trials that recorded none:\n%s", buf.String())
	}
}

// grewLine is the line of the report that says how each phase moved.
func grewSection(out string) string {
	_, s, ok := strings.Cut(out, "where the time grew")
	if !ok {
		return ""
	}
	return s
}

func TestTheReportSaysWhereTheTimeGrewBetweenTheAnswerAndTheFirstFailure(t *testing.T) {
	out := text(t, phased(0.1, 20, 10), false)
	s := grewSection(out)
	if s == "" {
		t.Fatalf("no section on where the time grew:\n%s", out)
	}
	for _, want := range []string{"700 rps", "750 rps", "wait +15.0ms", "queue 0ns", "connect +10.0ms", "most of it is wait"} {
		if !strings.Contains(s, want) {
			t.Errorf("section is missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "-workers") {
		t.Errorf("the server's wait grew and few requests were in flight, but the section points at the workers:\n%s", s)
	}
}

func TestTheReportSaysWhenRequestsQueuedBecauseEveryWorkerWasBusy(t *testing.T) {
	// At 750 rps a request took 310.25ms once sent (connect, send, wait and
	// transfer), so about 750 × 0.31025 = 233 were in flight on average: more
	// than the 200 workers.
	out := text(t, phased(40, 300, 10), false)
	s := grewSection(out)
	if !strings.Contains(s, "most of it is wait") || !strings.Contains(s, "about 233 requests") || !strings.Contains(s, "-workers") {
		t.Errorf("the section does not say the 200 workers were too few for 233 in flight:\n%s", s)
	}

	// 750 × 0.03025 = 23 in flight on average, but queue is what grew: requests
	// waited inside Gmeter (bursts fill the workers long before the average does),
	// while the server's own time hardly moved.
	out = text(t, phased(40, 20, 10), false)
	s = grewSection(out)
	if !strings.Contains(s, "most of it is queue") || !strings.Contains(s, "Gmeter's limit") || !strings.Contains(s, "more -workers") {
		t.Errorf("queue grew most, but the section does not say Gmeter may be the limit:\n%s", s)
	}
	if strings.Contains(s, "in flight on average") {
		t.Errorf("23 in flight of 200 workers, but the section says the average exceeded them:\n%s", s)
	}
}

func TestNoSectionOnWhereTheTimeGrewWithoutPhasesOrABracket(t *testing.T) {
	if s := grewSection(text(t, found(), false)); s != "" {
		t.Errorf("a section on phases although no trial has any:\n%s", s)
	}
	o := phased(40, 20, 10)
	o.NextFail = 0 // the search stopped at its maximum: nothing failed
	o.LowerBound = true
	if s := grewSection(text(t, o, false)); s != "" {
		t.Errorf("a section on where time grew although no trial failed:\n%s", s)
	}
}

func TestTheReportSaysWhenNoPhaseGrewOnAverage(t *testing.T) {
	s := grewSection(text(t, phased(0.1, 5, 0), false)) // 750 rps the same as 700
	if !strings.Contains(s, "no phase grew on average") {
		t.Errorf("nothing grew, but the section does not say so:\n%s", s)
	}
	if strings.Contains(s, "most of it") {
		t.Errorf("nothing grew, but the section names a phase:\n%s", s)
	}
}
