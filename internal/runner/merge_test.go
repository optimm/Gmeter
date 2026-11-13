package runner

import (
	"context"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/pacer"
)

// histOf records each duration once.
func histOf(ds ...time.Duration) *hist.Histogram {
	h := hist.New()
	for _, d := range ds {
		h.Record(d)
	}
	return h
}

// repeat is n copies of d.
func repeat(n int, d time.Duration) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = d
	}
	return out
}

// generatorResult is one generator's share of a run: n responses of lat each,
// with fail transport failures.
func generatorResult(n int, lat time.Duration, fail uint64, label string, elapsed time.Duration) *Result {
	c := histOf(repeat(n, lat)...)
	r := &Result{
		Elapsed:   elapsed,
		Scheduled: uint64(n) + fail,
		Dropped:   1,
		Statuses:  map[int]uint64{200: uint64(n)},
		Errors:    map[string]uint64{"timeout": fail},
		Protocols: map[string]uint64{"HTTP/1.1": uint64(n)},
		Corrected: c,
		Service:   histOf(repeat(n, lat/2)...),
		TimerLag:  histOf(lat / 100),
		Late:      2,
		Logged:    LoggedStatuses{Compared: uint64(n), Changes: []StatusChange{{Logged: 2, Got: 5, Count: 3}}},
		Timeline: []Window{
			{End: time.Second, Interval: time.Second, Scheduled: uint64(n), Responses: uint64(n), Failed: fail, RPS: float64(n), Latency: histOf(repeat(n, lat)...)},
		},
		Stages: []StageResult{{
			Stage: pacer.Stage{Duration: time.Second, From: 50, To: 50}, Scheduled: uint64(n) + fail, Failed: fail, Dropped: 1, BadStatus: 4,
			Corrected: histOf(repeat(n, lat)...), Service: histOf(repeat(n, lat/2)...),
			Phases: []PhaseTotal{{Phase: "queue", Requests: uint64(n), Total: time.Duration(n) * lat}},
		}},
		Labels: []LabelResult{{
			Label: label, Scheduled: uint64(n) + fail, Failed: fail, BadStatus: 4,
			Corrected: histOf(repeat(n, lat)...), Service: histOf(repeat(n, lat/2)...),
			Phases: []PhaseTotal{{Phase: "wait", Requests: uint64(n), Total: time.Duration(n) * lat}},
			Wait:   histOf(repeat(n, lat)...), LoggedCompared: uint64(n), LoggedDiffer: 3,
		}},
		Phases: []PhaseResult{{Phase: "wait", Samples: histOf(repeat(n, lat)...)}},
	}
	return r
}

func TestMergeAddsTheGeneratorsUpAsIfOneHadSentEverything(t *testing.T) {
	// 98 fast responses on one generator and 2 slow ones on the other: the
	// merged p99 is the slow one, which averaging the two p99s would not say.
	// Name order is the wrong order: the busiest label must come first.
	a := generatorResult(98, 10*time.Millisecond, 1, "GET /z", 3*time.Second)
	b := generatorResult(2, 500*time.Millisecond, 0, "GET /b", 2*time.Second)
	a.Timeline[0].Partial = true // a run's closing window

	m, err := Merge(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Scheduled != 101 || m.Dropped != 2 || m.Late != 4 || m.Elapsed != 3*time.Second {
		t.Errorf("scheduled %d dropped %d late %d elapsed %v, want 101, 2, 4, 3s", m.Scheduled, m.Dropped, m.Late, m.Elapsed)
	}
	if m.Statuses[200] != 100 || m.Errors["timeout"] != 1 || m.Protocols["HTTP/1.1"] != 100 {
		t.Errorf("statuses %v errors %v protocols %v", m.Statuses, m.Errors, m.Protocols)
	}
	if m.Corrected.Count() != 100 || m.Corrected.Quantile(0.99) < 490*time.Millisecond {
		t.Errorf("corrected: %d samples, p99 %v; want 100 and the slow 500ms", m.Corrected.Count(), m.Corrected.Quantile(0.99))
	}
	if m.Service.Count() != 100 || m.TimerLag.Count() != 2 {
		t.Errorf("service %d samples, timer lag %d, want 100 and 2", m.Service.Count(), m.TimerLag.Count())
	}
	if m.Logged.Compared != 100 || len(m.Logged.Changes) != 1 || m.Logged.Changes[0].Count != 6 {
		t.Errorf("logged = %+v, want 100 compared and one change of 6", m.Logged)
	}

	if len(m.Timeline) != 1 {
		t.Fatalf("%d windows, want 1", len(m.Timeline))
	}
	w := m.Timeline[0]
	if w.Scheduled != 100 || w.Responses != 100 || w.Failed != 1 || w.RPS != 100 || !w.Partial || w.P99 < 490*time.Millisecond || w.Max < 490*time.Millisecond {
		t.Errorf("window = %+v, want 100 responses at 100 rps and the slow 500ms as its p99", w)
	}

	if len(m.Stages) != 1 {
		t.Fatalf("%d stages, want 1", len(m.Stages))
	}
	st := m.Stages[0]
	if st.Stage.From != 100 || st.Stage.To != 100 || st.Stage.Duration != time.Second {
		t.Errorf("stage = %v, want 1s at 100 rps: each generator ran half the rate", st.Stage)
	}
	if st.Scheduled != 101 || st.Failed != 1 || st.Dropped != 2 || st.BadStatus != 8 || st.Corrected.Count() != 100 || st.Phases[0].Requests != 100 {
		t.Errorf("stage = %+v", st)
	}

	if len(m.Labels) != 2 || m.Labels[0].Label != "GET /z" || m.Labels[1].Label != "GET /b" {
		t.Fatalf("labels = %v, want GET /z then GET /b, busiest first", labelsOf(m))
	}
	if m.Labels[0].Wait.Count() != 98 || m.Labels[0].LoggedDiffer != 3 {
		t.Errorf("GET /z = %+v", m.Labels[0])
	}
	if len(m.Phases) != 1 || m.Phases[0].Samples.Count() != 100 {
		t.Errorf("phases = %v", m.Phases)
	}

	// The inputs are left as they were.
	if a.Corrected.Count() != 98 || a.Statuses[200] != 98 {
		t.Errorf("Merge changed its input: %d samples, statuses %v", a.Corrected.Count(), a.Statuses)
	}
}

func TestMergeCombinesTheSameLabelFromSeveralGenerators(t *testing.T) {
	a := generatorResult(10, 10*time.Millisecond, 0, "GET /a", time.Second)
	b := generatorResult(30, 20*time.Millisecond, 2, "GET /a", time.Second)
	m, err := Merge(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Labels) != 1 || m.Labels[0].Scheduled != 42 || m.Labels[0].Failed != 2 || m.Labels[0].Corrected.Count() != 40 ||
		m.Labels[0].Phases[0].Requests != 40 || m.Labels[0].LoggedCompared != 40 || m.Labels[0].LoggedDiffer != 6 ||
		m.Labels[0].BadStatus != 8 || m.Labels[0].Wait.Count() != 40 {
		t.Errorf("labels = %+v, want one GET /a with 42 scheduled, 2 failed, 40 answered", m.Labels)
	}
}

func TestMergeLinesUpWindowsByTimeEvenIfOneGeneratorRanLonger(t *testing.T) {
	a := generatorResult(10, 10*time.Millisecond, 0, "x", time.Second)
	b := generatorResult(10, 10*time.Millisecond, 0, "x", 2*time.Second)
	b.Timeline = append(b.Timeline, Window{End: 1500 * time.Millisecond, Interval: 500 * time.Millisecond, Partial: true, Responses: 5, RPS: 10, Latency: histOf(repeat(5, 30*time.Millisecond)...)})
	m, err := Merge(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Timeline) != 2 || m.Timeline[1].Responses != 5 || !m.Timeline[1].Partial || m.Timeline[1].P50 < 29*time.Millisecond {
		t.Errorf("timeline = %+v, want the second window as b had it", m.Timeline)
	}
}

func TestMergeRefusesWhatCannotBeAddedUp(t *testing.T) {
	a := generatorResult(10, 10*time.Millisecond, 0, "x", time.Second)
	b := generatorResult(10, 10*time.Millisecond, 0, "x", time.Second)
	b.Stages = append(b.Stages, b.Stages[0])
	if _, err := Merge(a, b); err == nil {
		t.Error("merged results with different numbers of stages")
	}
	long := generatorResult(10, 10*time.Millisecond, 0, "x", time.Second)
	long.Stages[0].Stage.Duration = 2 * time.Second
	if _, err := Merge(a, long); err == nil {
		t.Error("merged results whose stages ran for different lengths of time")
	}
	c := generatorResult(10, 10*time.Millisecond, 0, "x", time.Second)
	c.Timeline[0].Latency = nil // a window without its distribution cannot be merged honestly
	if _, err := Merge(a, c); err == nil {
		t.Error("merged a window without its latency distribution")
	}
	d := generatorResult(10, 10*time.Millisecond, 0, "x", time.Second)
	d.Phases = nil
	if _, err := Merge(a, d); err == nil {
		t.Error("merged a result with phases and one without")
	}
	if _, err := Merge(); err == nil {
		t.Error("merged nothing")
	}
}

func TestWindowsKeepTheirDistributionOnlyWhenAskedFor(t *testing.T) {
	srv := okServer(t)
	for _, keep := range []bool{true, false} {
		cfg := baseConfig(srv.URL)
		cfg.SampleEvery = 100 * time.Millisecond
		cfg.WindowHistograms = keep
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		var total uint64
		for i, w := range res.Timeline {
			switch {
			case !keep && w.Latency != nil:
				t.Errorf("window %d keeps a distribution although not asked to", i+1)
			case keep && (w.Latency == nil || w.Latency.Count() != w.Responses):
				t.Errorf("window %d: distribution %v, want one of its %d responses", i+1, w.Latency, w.Responses)
			case keep:
				total += w.Latency.Count()
			}
		}
		if keep && total != res.Corrected.Count() {
			t.Errorf("the windows' distributions hold %d samples, the run %d", total, res.Corrected.Count())
		}
	}
}
