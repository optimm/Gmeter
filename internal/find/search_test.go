package find

import (
	"context"
	"errors"
	"testing"
)

// capacity is a fake server that meets its target at any rate up to limit.
func capacity(limit float64) Probe {
	return func(_ context.Context, rate float64) (Verdict, any, error) {
		if rate <= limit {
			return Pass, nil, nil
		}
		return Fail, nil, nil
	}
}

func cfg() Config { return Config{Start: 100, Max: 100000, Floor: 1, Tolerance: 0.05} }

func rates(o Outcome, phase Phase) []float64 {
	var out []float64
	for _, tr := range o.Trials {
		if tr.Phase == phase {
			out = append(out, tr.Rate)
		}
	}
	return out
}

func equal(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSearchDoublesUntilTheTargetFailsThenBisects(t *testing.T) {
	o, err := Search(context.Background(), cfg(), capacity(730))
	if err != nil {
		t.Fatal(err)
	}
	if got := rates(o, Grow); !equal(got, []float64{100, 200, 400, 800}) {
		t.Fatalf("growth trials = %v, want 100 200 400 800", got)
	}
	// The answer is the last rate that passed and the next one up failed, with
	// the gap between them no wider than the tolerance.
	if o.Best > 730 || o.NextFail <= 730 {
		t.Fatalf("Best = %v, NextFail = %v: they must bracket the capacity of 730", o.Best, o.NextFail)
	}
	if width := (o.NextFail - o.Best) / o.Best; width > 0.05 {
		t.Fatalf("the bracket (%v, %v) is %.1f%% wide, want at most 5%%", o.Best, o.NextFail, width*100)
	}
	if o.LowerBound || o.GeneratorLimited {
		t.Errorf("neither flag applies: %+v", o)
	}
}

func TestSearchDescendsWhenTheStartingRateAlreadyFails(t *testing.T) {
	o, err := Search(context.Background(), cfg(), capacity(30))
	if err != nil {
		t.Fatal(err)
	}
	if got := rates(o, Descend); !equal(got, []float64{50, 25}) {
		t.Fatalf("descent trials = %v, want 50 then 25 (after the failing 100)", got)
	}
	if o.Best > 30 || o.NextFail <= 30 || (o.NextFail-o.Best)/o.Best > 0.05 {
		t.Fatalf("Best = %v, NextFail = %v: want a 5%% bracket around 30", o.Best, o.NextFail)
	}
}

func TestSearchReportsWhenNoRateMeetsTheTarget(t *testing.T) {
	o, err := Search(context.Background(), cfg(), capacity(0))
	if err != nil {
		t.Fatal(err)
	}
	if o.Best != 0 {
		t.Fatalf("Best = %v, want 0: nothing passes", o.Best)
	}
	last := o.Trials[len(o.Trials)-1]
	if last.Rate != 1 || len(o.Trials) > 10 {
		t.Fatalf("the descent should stop at the floor of 1 rps in a handful of trials, got %d trials ending at %v", len(o.Trials), last.Rate)
	}
	if o.NextFail != 1 {
		t.Errorf("NextFail = %v, want the lowest rate tried, 1", o.NextFail)
	}
}

func TestSearchStopsAtTheMaximumAndSaysItIsALowerBound(t *testing.T) {
	c := cfg()
	c.Max = 1000
	o, err := Search(context.Background(), c, capacity(1e9))
	if err != nil {
		t.Fatal(err)
	}
	if got := rates(o, Grow); !equal(got, []float64{100, 200, 400, 800, 1000}) {
		t.Fatalf("growth = %v, want doubling clamped to the max of 1000", got)
	}
	if o.Best != 1000 || !o.LowerBound || o.NextFail != 0 || len(rates(o, Bisect)) != 0 {
		t.Fatalf("want Best 1000 as a lower bound with nothing failing and no bisection: %+v", o)
	}
}

func TestSearchConfirmsTheAnswerAndCountsHowOftenItHeld(t *testing.T) {
	c := cfg()
	c.Confirm = 3
	calls := 0
	probe := func(ctx context.Context, rate float64) (Verdict, any, error) {
		v, d, err := capacity(730)(ctx, rate)
		if v == Pass && rate > 600 { // a rate that passes only sometimes, like a noisy server
			calls++
			if calls%2 == 0 {
				v = Fail
			}
		}
		return v, d, err
	}
	o, err := Search(context.Background(), c, probe)
	if err != nil {
		t.Fatal(err)
	}
	confirms := rates(o, Confirm)
	if len(confirms) != 3 || o.ConfirmRuns != 3 {
		t.Fatalf("%d confirmation trials (ConfirmRuns %d), want 3", len(confirms), o.ConfirmRuns)
	}
	for _, r := range confirms {
		if r != o.Best {
			t.Errorf("a confirmation ran at %v, want the answer %v", r, o.Best)
		}
	}
	passed := 0
	for _, tr := range o.Trials {
		if tr.Phase == Confirm && tr.Verdict == Pass {
			passed++
		}
	}
	if o.Confirmed != passed {
		t.Fatalf("Confirmed = %d, but %d confirmation trials passed", o.Confirmed, passed)
	}
}

func TestAGeneratorLimitedTrialCountsAsAFailureButIsFlagged(t *testing.T) {
	probe := func(_ context.Context, rate float64) (Verdict, any, error) {
		if rate > 500 {
			return Limited, nil, nil // the target looked met, but the generator could not keep time
		}
		return Pass, nil, nil
	}
	o, err := Search(context.Background(), cfg(), probe)
	if err != nil {
		t.Fatal(err)
	}
	if o.Best > 500 || o.NextFail <= 500 {
		t.Fatalf("Best = %v, NextFail = %v: a limited trial must bracket like a failure", o.Best, o.NextFail)
	}
	if !o.GeneratorLimited {
		t.Fatal("the failing end of the bracket was generator-limited, and the outcome must say so")
	}
}

func TestSearchStopsWhenCancelledAndKeepsWhatItLearned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	probe := func(ctx context.Context, rate float64) (Verdict, any, error) {
		if n++; n == 3 {
			cancel()
		}
		return capacity(730)(ctx, rate)
	}
	o, err := Search(ctx, cfg(), probe)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(o.Trials) != 3 {
		t.Fatalf("%d trials kept, want the 3 that ran", len(o.Trials))
	}
	if o.Best != 400 {
		t.Errorf("Best = %v, want 400, the highest rate that passed before the stop (100, 200, 400 all pass)", o.Best)
	}
}

func TestAProbeErrorEndsTheSearchWithTheTrialsSoFar(t *testing.T) {
	boom := errors.New("boom")
	n := 0
	probe := func(ctx context.Context, rate float64) (Verdict, any, error) {
		if n++; n == 2 {
			return Fail, nil, boom
		}
		return capacity(730)(ctx, rate)
	}
	o, err := Search(context.Background(), cfg(), probe)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the probe's error", err)
	}
	if len(o.Trials) != 1 || o.Best != 100 {
		t.Fatalf("want the one completed trial kept: %+v", o)
	}
}

func TestSearchRejectsNonsenseConfigs(t *testing.T) {
	bad := map[string]func(*Config){
		"no start":         func(c *Config) { c.Start = 0 },
		"max below start":  func(c *Config) { c.Max = 50 },
		"zero tolerance":   func(c *Config) { c.Tolerance = 0 },
		"huge tolerance":   func(c *Config) { c.Tolerance = 1.5 },
		"negative floor":   func(c *Config) { c.Floor = -1 },
		"negative confirm": func(c *Config) { c.Confirm = -1 },
	}
	for name, mutate := range bad {
		c := cfg()
		mutate(&c)
		if _, err := Search(context.Background(), c, capacity(100)); err == nil {
			t.Errorf("%s: Search succeeded, want an error", name)
		}
	}
}
