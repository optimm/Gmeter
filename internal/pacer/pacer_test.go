package pacer

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type due struct {
	at    time.Duration // offset from t0
	stage int
}

// drain returns every request the schedule produces.
func drain(t *testing.T, stages []Stage, a Arrivals, seed uint64) []due {
	t.Helper()
	s, err := New(t0, stages, a, seed)
	if err != nil {
		t.Fatal(err)
	}
	var out []due
	for {
		at, stage, ok := s.Next()
		if !ok {
			return out
		}
		out = append(out, due{at.Sub(t0), stage})
	}
}

func within(got, want, tol time.Duration) bool { return (got - want).Abs() <= tol }

func TestFirstRequestIsDueAtStart(t *testing.T) {
	got := drain(t, ConstantRate(100, time.Second), Even, 0)
	if got[0].at != 0 {
		t.Fatalf("first request due at %v, want the start", got[0].at)
	}
}

func TestRequestsAreSpacedByTheRateInterval(t *testing.T) {
	got := drain(t, ConstantRate(100, 5*time.Second), Even, 0) // one every 10ms
	if got[1].at != 10*time.Millisecond {
		t.Fatalf("request 1 due at %v, want 10ms", got[1].at)
	}
	if got[250].at != 2500*time.Millisecond {
		t.Fatalf("request 250 due at %v, want 2.5s", got[250].at)
	}
}

func TestAConstantRateSchedulesExactlyRateTimesDuration(t *testing.T) {
	got := drain(t, ConstantRate(100, 500*time.Millisecond), Even, 0)
	if len(got) != 50 {
		t.Fatalf("%d requests, want 50", len(got))
	}
	if last := got[len(got)-1].at; last >= 500*time.Millisecond {
		t.Fatalf("last request due at %v, at or after the end of the schedule", last)
	}
}

func TestScheduleDoesNotDriftWhenRateDoesNotDivideASecond(t *testing.T) {
	// 3 rps => 333,333,333.33ns per request. Accumulating a truncated
	// interval would drift by 1ns per request; computing from the request's
	// index must not.
	s, err := New(t0, ConstantRate(3, 1_000_000*time.Second), Even, 0)
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for {
		at, _, ok := s.Next()
		if !ok {
			break
		}
		if n == 3 && at.Sub(t0) != time.Second {
			t.Fatalf("request 3 due at %v, want exactly 1s", at.Sub(t0))
		}
		if n == 3_000_000-1 {
			want := 999_999*time.Second + 666_666_667*time.Nanosecond
			if diff := (at.Sub(t0) - want).Abs(); diff > time.Nanosecond {
				t.Fatalf("request %d off by %v, want <=1ns (no accumulated drift)", n, diff)
			}
		}
		n++
	}
	if n != 3_000_000 {
		t.Fatalf("%d requests, want 3,000,000", n)
	}
}

func TestInvalidProfilesAreRejected(t *testing.T) {
	cases := map[string][]Stage{
		"no stages":        nil,
		"zero rate":        ConstantRate(0, time.Second),
		"negative rate":    ConstantRate(-5, time.Second),
		"NaN rate":         ConstantRate(math.NaN(), time.Second),
		"infinite rate":    ConstantRate(math.Inf(1), time.Second),
		"zero duration":    ConstantRate(10, 0),
		"negative length":  ConstantRate(10, -time.Second),
		"negative ramp to": {{Duration: time.Second, From: 10, To: -1}},
		"NaN ramp to":      {{Duration: time.Second, From: 10, To: math.NaN()}},
		"one bad stage":    {{Duration: time.Second, From: 10, To: 10}, {Duration: 0, From: 10, To: 10}},
	}
	for name, stages := range cases {
		if _, err := New(t0, stages, Even, 0); err == nil {
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
	if _, err := New(t0, ConstantRate(10, time.Second), Arrivals(99), 0); err == nil {
		t.Error("an unknown arrival process was accepted")
	}
}

func TestAStepProfileSwitchesRateAtTheStageBoundary(t *testing.T) {
	got := drain(t, []Stage{
		{Duration: time.Second, From: 100, To: 100},
		{Duration: time.Second, From: 200, To: 200},
	}, Even, 0)
	if len(got) != 300 {
		t.Fatalf("%d requests, want 100 + 200", len(got))
	}
	if got[1].at != 10*time.Millisecond || got[1].stage != 0 {
		t.Errorf("request 1 = %+v, want 10ms into stage 0", got[1])
	}
	// The 101st request is the first of stage 1, due exactly at the boundary,
	// and from there they are 5ms apart.
	if got[100].at != time.Second || got[100].stage != 1 {
		t.Errorf("request 100 = %+v, want exactly 1s, stage 1", got[100])
	}
	if got[101].at != time.Second+5*time.Millisecond {
		t.Errorf("request 101 due at %v, want 1.005s", got[101].at)
	}
	if got[99].stage != 0 {
		t.Errorf("request 99 is in stage %d, want 0", got[99].stage)
	}
}

func TestARampSpacesRequestsByTheCumulativeCount(t *testing.T) {
	// 0 -> 100 rps over 10s: the rate at time t is 10t, so the expected number
	// of requests by t is 5t². Request k is therefore due at sqrt(k/5).
	got := drain(t, []Stage{{Duration: 10 * time.Second, From: 0, To: 100}}, Even, 0)
	if len(got) != 500 {
		t.Fatalf("%d requests, want 500 (the area under the ramp)", len(got))
	}
	for _, k := range []int{0, 20, 125, 499} {
		want := time.Duration(math.Sqrt(float64(k)/5) * float64(time.Second))
		if !within(got[k].at, want, time.Microsecond) {
			t.Errorf("request %d due at %v, want %v", k, got[k].at, want)
		}
	}

	// And down: 100 -> 0 over 10s, expected count 100t - 5t². 375 requests have
	// been due by t = 5s.
	down := drain(t, []Stage{{Duration: 10 * time.Second, From: 100, To: 0}}, Even, 0)
	if len(down) != 500 {
		t.Fatalf("%d requests on the way down, want 500", len(down))
	}
	if !within(down[375].at, 5*time.Second, time.Microsecond) {
		t.Errorf("request 375 due at %v, want 5s", down[375].at)
	}
}

func TestDueTimesNeverGoBackwards(t *testing.T) {
	stages := []Stage{
		{Duration: 3 * time.Second, From: 50, To: 400},
		{Duration: 2 * time.Second, From: 400, To: 400},
		{Duration: 3 * time.Second, From: 400, To: 5},
	}
	for _, a := range []Arrivals{Even, Poisson} {
		prev := time.Duration(-1)
		for i, d := range drain(t, stages, a, 7) {
			if d.at < prev {
				t.Fatalf("%v: request %d due at %v, before the previous one at %v", a, i, d.at, prev)
			}
			prev = d.at
		}
	}
}

func TestAPauseStageSchedulesNothing(t *testing.T) {
	got := drain(t, []Stage{
		{Duration: time.Second, From: 100, To: 100},
		{Duration: time.Second, From: 0, To: 0},
		{Duration: time.Second, From: 100, To: 100},
	}, Even, 0)
	if len(got) != 200 {
		t.Fatalf("%d requests, want 200", len(got))
	}
	for _, d := range got {
		if d.at >= time.Second && d.at < 2*time.Second {
			t.Fatalf("a request is due at %v, during the pause", d.at)
		}
	}
	if got[100].at != 2*time.Second || got[100].stage != 2 {
		t.Errorf("first request after the pause = %+v, want exactly 2s in stage 2", got[100])
	}
}

func TestAFractionalCountCarriesAcrossStages(t *testing.T) {
	// 2.5 rps for 1s is 2.5 requests. Rounding each stage would lose half a
	// request at the boundary; the schedule counts continuously instead.
	got := drain(t, []Stage{
		{Duration: time.Second, From: 2.5, To: 2.5},
		{Duration: time.Second, From: 2.5, To: 2.5},
	}, Even, 0)
	if len(got) != 5 {
		t.Fatalf("%d requests, want 5", len(got))
	}
	if got[2].at != 800*time.Millisecond || got[2].stage != 0 {
		t.Errorf("request 2 = %+v, want 0.8s, stage 0", got[2])
	}
	if got[3].at != 1200*time.Millisecond || got[3].stage != 1 {
		t.Errorf("request 3 = %+v, want 1.2s, stage 1", got[3])
	}
}

func TestDurationAndExpectedCount(t *testing.T) {
	s, err := New(t0, []Stage{
		{Duration: 10 * time.Second, From: 0, To: 100},
		{Duration: 5 * time.Second, From: 100, To: 100},
	}, Even, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Duration() != 15*time.Second {
		t.Errorf("Duration = %v, want 15s", s.Duration())
	}
	if got := s.Expected(); math.Abs(got-1000) > 1e-9 { // 500 under the ramp + 500 flat
		t.Errorf("Expected = %v, want 1000", got)
	}
}

func TestPoissonArrivalsAreRandomButReproducibleFromTheSeed(t *testing.T) {
	a := drain(t, ConstantRate(1000, 2*time.Second), Poisson, 42)
	b := drain(t, ConstantRate(1000, 2*time.Second), Poisson, 42)
	c := drain(t, ConstantRate(1000, 2*time.Second), Poisson, 43)
	if len(a) != len(b) {
		t.Fatalf("same seed gave %d and %d requests", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed diverged at request %d: %v vs %v", i, a[i], b[i])
		}
	}
	same := len(a) == len(c)
	for i := 0; same && i < len(a); i++ {
		same = a[i] == c[i]
	}
	if same {
		t.Fatal("a different seed gave the identical schedule")
	}
}

// A Poisson process at rate r has exponential gaps: mean 1/r, and a standard
// deviation equal to the mean. Evenly spaced arrivals have a standard
// deviation of zero, which is exactly why they understate queueing.
func TestPoissonGapsAreExponential(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		got := drain(t, ConstantRate(1000, 20*time.Second), Poisson, seed)
		if n := len(got); n < 19_300 || n > 20_700 { // 20,000 +- ~5 standard deviations
			t.Fatalf("seed %d: %d requests, want about 20,000", seed, n)
		}
		var sum, sumSq float64
		for i := 1; i < len(got); i++ {
			gap := float64(got[i].at - got[i-1].at)
			sum += gap
			sumSq += gap * gap
		}
		n := float64(len(got) - 1)
		mean := sum / n
		std := math.Sqrt(sumSq/n - mean*mean)
		if mean < 0.97e6 || mean > 1.03e6 {
			t.Errorf("seed %d: mean gap %.0fns, want about 1,000,000", seed, mean)
		}
		if cv := std / mean; cv < 0.95 || cv > 1.05 {
			t.Errorf("seed %d: gap std/mean = %.3f, want about 1 (exponential)", seed, cv)
		}
	}
}

func TestPoissonFollowsAChangingRate(t *testing.T) {
	got := drain(t, []Stage{
		{Duration: 10 * time.Second, From: 100, To: 100},
		{Duration: 10 * time.Second, From: 300, To: 300},
	}, Poisson, 3)
	var perStage [2]int
	for _, d := range got {
		perStage[d.stage]++
		if inStage := d.at >= 10*time.Second; inStage != (d.stage == 1) {
			t.Fatalf("request at %v is reported in stage %d", d.at, d.stage)
		}
	}
	if perStage[0] < 900 || perStage[0] > 1100 || perStage[1] < 2700 || perStage[1] > 3300 {
		t.Fatalf("requests per stage = %v, want about [1000 3000]", perStage)
	}
}
