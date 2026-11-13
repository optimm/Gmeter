package pacer

import (
	"math"
	"slices"
	"testing"
	"time"
)

func TestSchedulesAtAShareOfTheRateInterleaveIntoTheWholeSchedule(t *testing.T) {
	// A ramp, a step and a pause, so the gap between requests changes all the time.
	whole := []Stage{{Duration: 2 * time.Second, From: 0, To: 90}, {Duration: time.Second, From: 30, To: 30}, {Duration: time.Second}, {Duration: time.Second, From: 60, To: 6}}
	const n = 3
	want := drain(t, whole, Even, 0)

	share := make([]Stage, len(whole))
	for i, st := range whole {
		share[i] = Stage{Duration: st.Duration, From: st.From / n, To: st.To / n}
	}
	var got []time.Duration
	for i := range n {
		s, err := New(t0, share, Even, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetPhase(float64(i) / n); err != nil {
			t.Fatal(err)
		}
		for {
			at, _, ok := s.Next()
			if !ok {
				break
			}
			got = append(got, at.Sub(t0))
		}
	}
	slices.Sort(got)
	if len(got) != len(want) {
		t.Fatalf("the shares scheduled %d requests together, the whole schedule %d", len(got), len(want))
	}
	for k := range want {
		if !within(got[k], want[k].at, time.Microsecond) {
			t.Fatalf("request %d: due at %v in the shares, %v in the whole schedule", k, got[k], want[k].at)
		}
	}
}

func TestAPhaseDelaysTheFirstRequestByThatFractionOfAGap(t *testing.T) {
	s, err := New(t0, ConstantRate(10, time.Second), Even, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPhase(0.25); err != nil {
		t.Fatal(err)
	}
	at, _, _ := s.Next()
	next, _, _ := s.Next()
	if at.Sub(t0) != 25*time.Millisecond || next.Sub(at) != 100*time.Millisecond {
		t.Errorf("first due at %v, then %v later; want 25ms, then 100ms", at.Sub(t0), next.Sub(at))
	}
}

func TestAPhaseIsAFractionOfAnEvenGap(t *testing.T) {
	for _, p := range []float64{-0.1, 1, 1.5, math.NaN(), math.Inf(1)} {
		s, _ := New(t0, ConstantRate(10, time.Second), Even, 0)
		if err := s.SetPhase(p); err == nil {
			t.Errorf("phase %v accepted", p)
		}
	}
	s, _ := New(t0, ConstantRate(10, time.Second), Poisson, 1)
	if err := s.SetPhase(0.5); err == nil {
		t.Error("a phase accepted for random arrivals, which have no gap to shift by")
	}
	s, _ = New(t0, ConstantRate(10, time.Second), Even, 0)
	s.Next()
	if err := s.SetPhase(0.5); err == nil {
		t.Error("a phase accepted after the schedule had started")
	}
}
