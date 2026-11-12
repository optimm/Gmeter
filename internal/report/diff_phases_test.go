package report

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/runner"
)

// phasedRun is a run of n answered requests that each spent the given time in
// each phase; connects of them also spent 4ms connecting.
func phasedRun(n int, connects int, queue, send, wait, transfer time.Duration) Summary {
	phase := func(name string, count int, each time.Duration) runner.PhaseResult {
		h := hist.New()
		for range count {
			h.Record(each)
		}
		return runner.PhaseResult{Phase: name, Samples: h}
	}
	s := mkSummary(100*time.Millisecond, 2)
	s.Phases = phaseSummaries([]runner.PhaseResult{
		phase("queue", n, queue),
		phase("dns", 0, 0),
		phase("connect", connects, 4*time.Millisecond),
		phase("tls", 0, 0),
		phase("send", n, send),
		phase("wait", n, wait),
		phase("transfer", n, transfer),
	})
	return s
}

func comparedPhases(t *testing.T, a, b Summary) Comparison {
	t.Helper()
	c, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func phaseRowNamed(t *testing.T, c Comparison, name string) PhaseRow {
	t.Helper()
	for _, r := range c.Phases {
		if r.Phase == name {
			return r
		}
	}
	t.Fatalf("no phase %q in %+v", name, c.Phases)
	return PhaseRow{}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A is 100 answered requests of 1ms queue, 0.1ms send, 8ms wait and 1ms
// transfer, 5 of which also connected (4ms); B is 50 answered requests whose
// wait is 40ms and that never connected. The runs answered different numbers of
// requests, so a time per request has to be of each run's own.
func aSlowerWaitRuns() (Summary, Summary) {
	return phasedRun(100, 5, time.Millisecond, 100*time.Microsecond, 8*time.Millisecond, time.Millisecond),
		phasedRun(50, 0, time.Millisecond, 100*time.Microsecond, 40*time.Millisecond, time.Millisecond)
}

func slowerWait(t *testing.T) Comparison {
	t.Helper()
	a, b := aSlowerWaitRuns()
	return comparedPhases(t, a, b)
}

func TestComparePhasesMeasuresTheTimePerAnsweredRequestInEachPhase(t *testing.T) {
	c := slowerWait(t)

	var names []string
	for _, r := range c.Phases {
		names = append(names, r.Phase)
	}
	// connect is in A only, and is still a row: its time per request dropped to nothing.
	if got := strings.Join(names, ","); got != "queue,connect,send,wait,transfer" {
		t.Fatalf("phases = %s, want those either run has, in the order a request goes through them", got)
	}

	wait := phaseRowNamed(t, c, "wait")
	if !near(wait.PerRequestA, 8) || !near(wait.PerRequestB, 40) || !near(wait.ChangeMs(), 32) {
		t.Errorf("wait per request = %v to %v (%+v), want 8ms to 40ms: of each run's own answered requests", wait.PerRequestA, wait.PerRequestB, wait.ChangeMs())
	}
	if !near(wait.P99A, 8) || !near(wait.P99B, 40) || !near(wait.P99ChangePct, 400) {
		t.Errorf("wait p99 = %v to %v (%+.1f%%), want 8ms to 40ms (+400%%)", wait.P99A, wait.P99B, wait.P99ChangePct)
	}
	// 1+0.2+0.1+8+1 = 10.3ms a request in A; in B 1+0.1+40+1 = 42.1ms.
	if want := 2000.0 / 2105 * 100; !near(wait.ShareB, want) || !near(wait.ShareA, 800.0/1030*100) {
		t.Errorf("wait shares = %v and %v, want %v and %v", wait.ShareA, wait.ShareB, 800.0/1030*100, want)
	}

	// Only the requests that connected had a connect phase, but a time per
	// request is of all of them: 5 of 100 took 4ms.
	conn := phaseRowNamed(t, c, "connect")
	if !near(conn.PerRequestA, 0.2) || conn.PerRequestB != 0 || !near(conn.ChangeMs(), -0.2) {
		t.Errorf("connect per request = %v to %v, want 0.2ms to nothing", conn.PerRequestA, conn.PerRequestB)
	}
	if !math.IsNaN(conn.P99ChangePct) || conn.SamplesB != 0 {
		t.Errorf("connect p99 change = %v with %d samples in B, want n/a: B has no p99 to compare", conn.P99ChangePct, conn.SamplesB)
	}
}

func TestComparePhasesNamesTheBiggestMover(t *testing.T) {
	c := slowerWait(t)

	main, ok := c.MainPhase()
	if !ok || main.Phase != "wait" {
		t.Fatalf("main phase = %q, %v, want wait: it moved 32ms a request of A's 10.3ms", main.Phase, ok)
	}
}

func TestComparePhasesDoesNotNameAPhaseForNoise(t *testing.T) {
	a := phasedRun(100, 0, time.Millisecond, 100*time.Microsecond, 8*time.Millisecond, time.Millisecond)
	// wait +0.5ms of A's 10.1ms mean: under the tenth that would count as a change.
	b := phasedRun(100, 0, time.Millisecond, 100*time.Microsecond, 8500*time.Microsecond, time.Millisecond)

	c := comparedPhases(t, a, b)
	if main, ok := c.MainPhase(); ok {
		t.Errorf("main phase = %q (%+.2fms), want none: no phase moved by a tenth of the mean", main.Phase, main.ChangeMs())
	}
}

func TestComparePhasesNamesAFallingPhaseToo(t *testing.T) {
	fast, slow := aSlowerWaitRuns()
	c := comparedPhases(t, slow, fast) // swapped: B is the run with the faster wait

	main, ok := c.MainPhase()
	if !ok || main.Phase != "wait" || main.ChangeMs() >= 0 {
		t.Errorf("main phase = %q moved %+.1fms, %v, want wait, falling", main.Phase, main.ChangeMs(), ok)
	}
}

func TestComparePhasesSaysWhenOnlyOneRunHasThem(t *testing.T) {
	a, _ := aSlowerWaitRuns()
	b := mkSummary(100*time.Millisecond, 2) // phases not recorded

	c := comparedPhases(t, a, b)
	if len(c.Phases) != 0 {
		t.Errorf("compared %d phases, want none: B has none to compare with", len(c.Phases))
	}
	if _, ok := c.MainPhase(); ok {
		t.Error("main phase named although nothing was compared")
	}
	if len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "only A") || !strings.Contains(c.Notes[0], "phase") {
		t.Errorf("notes = %q, want one saying that only A has phase timings", c.Notes)
	}
	if none := comparedPhases(t, b, b); len(none.Notes) != 0 || len(none.Phases) != 0 {
		t.Errorf("neither run has phases, yet notes = %q, phases = %d", none.Notes, len(none.Phases))
	}
}

// phaseTextRows returns the cells after the phase name of each row of the phase
// table: mean A, mean B, change, p99 A, p99 B, p99 change, share A, share B.
func phaseTextRows(out string) map[string][]string {
	rows := map[string][]string{}
	_, section, _ := strings.Cut(out, "where the time goes")
	for _, line := range strings.Split(section, "\n")[1:] {
		f := strings.Fields(line)
		if line == "" {
			break
		}
		if len(f) == 9 && strings.HasPrefix(line, "  ") && f[0] != "phase" {
			rows[f[0]] = f[1:]
		}
	}
	return rows
}

func TestPhaseDiffTableInTheTextReport(t *testing.T) {
	var buf strings.Builder
	if err := slowerWait(t).WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	rows := phaseTextRows(out)
	if got := strings.Join(rows["wait"], " "); got != "8.0ms 40.0ms +32.0ms 8.0ms 40.0ms +400.0% 77.7% 95.0%" {
		t.Errorf("wait row = %q", got)
	}
	// A phase that fell has a minus sign; one with no p99 in B has dashes and n/a.
	if got := strings.Join(rows["connect"], " "); got != "200µs 0ns -200µs 4.0ms - n/a 1.9% 0.0%" {
		t.Errorf("connect row = %q", got)
	}
	if got := strings.Join(rows["queue"], " "); !strings.HasPrefix(got, "1.0ms 1.0ms 0ns ") {
		t.Errorf("queue row = %q, want an unchanged 1.0ms and a change of 0ns", got)
	}
	if !strings.Contains(out, "biggest change: wait, +32.0ms per answered request (mean 10.3ms to 42.1ms)") {
		t.Errorf("no line naming the phase that moved most:\n%s", out)
	}
}

func TestPhaseDiffTextSaysWhenNothingMovedMuch(t *testing.T) {
	a := phasedRun(100, 0, time.Millisecond, 100*time.Microsecond, 8*time.Millisecond, time.Millisecond)
	var buf strings.Builder
	if err := comparedPhases(t, a, a).WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "biggest change") || !strings.Contains(out, "no phase moved by as much as 10% of A's mean") {
		t.Errorf("want a line saying no phase moved much, and none naming one:\n%s", out)
	}
}

func TestPhaseDiffTableIsLeftOutWithoutPhases(t *testing.T) {
	s := mkSummary(100*time.Millisecond, 2)
	var buf strings.Builder
	if err := comparedPhases(t, s, s).WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "where the time goes") {
		t.Errorf("a phase table although neither run has phases:\n%s", buf.String())
	}
}
