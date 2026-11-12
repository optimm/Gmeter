package find

import (
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

func TestParseSLO(t *testing.T) {
	cases := []struct {
		in      string
		q       float64
		latency time.Duration
	}{
		{"p99=50ms", 0.99, 50 * time.Millisecond},
		{"p99.9=200ms", 0.999, 200 * time.Millisecond},
		{"p50=5ms", 0.5, 5 * time.Millisecond},
		{" P95 = 1s ", 0.95, time.Second},
	}
	for _, c := range cases {
		got, err := ParseSLO(c.in)
		if err != nil {
			t.Errorf("ParseSLO(%q): %v", c.in, err)
			continue
		}
		if got.Quantile != c.q || got.Latency != c.latency {
			t.Errorf("ParseSLO(%q) = %+v, want p%v at %v", c.in, got, c.q*100, c.latency)
		}
	}
	for _, in := range []string{"", "50ms", "p99", "p99=", "p99=fast", "p99=0s", "p99=-5ms", "p0=5ms", "p100=5ms", "pNaN=5ms", "x99=5ms", "p99<5ms"} {
		if _, err := ParseSLO(in); err == nil {
			t.Errorf("ParseSLO(%q) succeeded, want an error", in)
		}
	}
	if s := (SLO{Quantile: 0.99, Latency: 50 * time.Millisecond}).String(); s != "p99 ≤ 50ms" {
		t.Errorf("String() = %q, want %q", s, "p99 ≤ 50ms")
	}
}

// trial builds the result of one trial: a stage whose scheduled requests all
// answered at lat (so every quantile equals lat), minus failed transport
// failures and bad bad-status responses.
func trial(scheduled int, lat time.Duration, failed, bad uint64, timerLag time.Duration) (runner.StageResult, *runner.Result) {
	c, sv := hist.New(), hist.New()
	for range int(uint64(scheduled) - failed) {
		c.Record(lat)
		sv.Record(lat)
	}
	lag := hist.New()
	for range 100 {
		lag.Record(timerLag)
	}
	st := runner.StageResult{
		Stage:     pacer.Stage{Duration: 10 * time.Second, From: 100, To: 100},
		Scheduled: uint64(scheduled), Failed: failed, BadStatus: bad, Corrected: c, Service: sv,
	}
	return st, &runner.Result{TimerLag: lag, Stages: []runner.StageResult{st}}
}

var slo = SLO{Quantile: 0.99, Latency: 50 * time.Millisecond, MaxErrorRate: 0.01}

func TestATrialWithinTheTargetPasses(t *testing.T) {
	st, res := trial(1000, 10*time.Millisecond, 0, 0, 50*time.Microsecond)
	v, o := Evaluate(slo, 100, st, res)
	if v != Pass || len(o.Reasons) != 0 {
		t.Fatalf("verdict %v, reasons %v: want a clean pass", v, o.Reasons)
	}
	if o.Observed < 9*time.Millisecond || o.Observed > 11*time.Millisecond || o.Rate != 100 || o.Scheduled != 1000 {
		t.Errorf("observation %+v: want p99 about 10ms at 100 rps over 1000 requests", o)
	}
}

func TestATrialOverTheLatencyTargetFailsAndSaysWhy(t *testing.T) {
	st, res := trial(1000, 80*time.Millisecond, 0, 0, 50*time.Microsecond)
	v, o := Evaluate(slo, 100, st, res)
	if v != Fail || len(o.Reasons) != 1 || !strings.Contains(o.Reasons[0], "p99") {
		t.Fatalf("verdict %v, reasons %v: want a failure naming p99", v, o.Reasons)
	}
}

func TestErrorsFailATrialWhateverTheLatency(t *testing.T) {
	// A server that fails fast has superb latency. Both kinds of error count.
	for name, st := range map[string]func() (runner.StageResult, *runner.Result){
		"transport failures": func() (runner.StageResult, *runner.Result) { return trial(1000, time.Millisecond, 20, 0, 0) },
		"bad statuses":       func() (runner.StageResult, *runner.Result) { return trial(1000, time.Millisecond, 0, 20, 0) },
		"a mix":              func() (runner.StageResult, *runner.Result) { return trial(1000, time.Millisecond, 8, 8, 0) },
	} {
		s, res := st()
		v, o := Evaluate(slo, 100, s, res)
		if v != Fail || len(o.Reasons) != 1 || !strings.Contains(o.Reasons[0], "errors") {
			t.Errorf("%s: verdict %v, reasons %v: want a failure naming errors", name, v, o.Reasons)
		}
		if o.ErrorRate < 0.015 || o.ErrorRate > 0.025 {
			t.Errorf("%s: error rate %v, want 0.02", name, o.ErrorRate)
		}
	}
	// Right at the budget is fine.
	s, res := trial(1000, time.Millisecond, 10, 0, 0)
	if v, _ := Evaluate(slo, 100, s, res); v != Pass {
		t.Errorf("1%% errors against a 1%% budget: verdict %v, want pass", v)
	}
}

func TestATrialThatSentNothingOrLeftRequestsUnsentFails(t *testing.T) {
	empty, res := trial(0, time.Millisecond, 0, 0, 0)
	if v, o := Evaluate(slo, 100, empty, res); v != Fail || len(o.Reasons) == 0 {
		t.Errorf("no requests: verdict %v, reasons %v, want a failure", v, o.Reasons)
	}
	st, res := trial(1000, time.Millisecond, 0, 0, 0)
	st.Dropped = 50
	if v, o := Evaluate(slo, 100, st, res); v != Fail || !strings.Contains(strings.Join(o.Reasons, " "), "never sent") {
		t.Errorf("dropped requests: verdict %v, reasons %v, want a failure saying they were never sent", v, o.Reasons)
	}
}

// If the generator could not keep time, a good-looking result is not evidence
// about the server: it is Limited, not a pass. A bad result stays a failure:
// the corrected latency is honest whatever the generator's state.
func TestAGeneratorThatCouldNotKeepTimeMakesAPassLimited(t *testing.T) {
	st, res := trial(1000, 10*time.Millisecond, 0, 0, 20*time.Millisecond)
	v, o := Evaluate(slo, 100, st, res)
	if v != Limited || len(o.Reasons) != 1 || !strings.Contains(o.Reasons[0], "timer lag") {
		t.Fatalf("verdict %v, reasons %v: want Limited citing timer lag", v, o.Reasons)
	}
	st, res = trial(1000, 80*time.Millisecond, 0, 0, 20*time.Millisecond)
	if v, _ := Evaluate(slo, 100, st, res); v != Fail {
		t.Errorf("a missed target is a failure even when the generator struggled, got %v", v)
	}
}

func TestTheQuantileComesFromTheTarget(t *testing.T) {
	c := hist.New()
	for range 900 {
		c.Record(2 * time.Millisecond)
	}
	for range 100 {
		c.Record(200 * time.Millisecond)
	}
	st := runner.StageResult{Scheduled: 1000, Corrected: c, Service: c}
	res := &runner.Result{TimerLag: hist.New(), Stages: []runner.StageResult{st}}

	if v, _ := Evaluate(SLO{Quantile: 0.5, Latency: 10 * time.Millisecond}, 100, st, res); v != Pass {
		t.Errorf("p50 is 2ms, within 10ms, but got %v", v)
	}
	if v, _ := Evaluate(SLO{Quantile: 0.95, Latency: 10 * time.Millisecond}, 100, st, res); v != Fail {
		t.Errorf("p95 is 200ms, over 10ms, but got %v", v)
	}
}
