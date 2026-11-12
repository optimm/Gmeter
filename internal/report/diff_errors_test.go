package report

import (
	"math"
	"strings"
	"testing"
	"time"
)

// withOutcomes is a run that sent n requests, of which failed failed and bad got
// a bad status.
func withOutcomes(n, failed, bad uint64) Summary {
	s := mkSummary(100*time.Millisecond, 2)
	s.Scheduled, s.Failed, s.BadStatus = n, failed, bad
	return s
}

func compareRuns(t *testing.T, a, b Summary) Comparison {
	t.Helper()
	c, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestErrorRiseIsInPercentagePointsOfEachRunsOwnRequests(t *testing.T) {
	// A: 10 of 1000 failed or were bad, 1.0%. B: 30 of 500, 6.0%.
	c := compareRuns(t, withOutcomes(1000, 5, 5), withOutcomes(500, 20, 10))

	got, ok := c.ErrorRise()
	if !ok || math.Abs(got-5) > 1e-9 {
		t.Errorf("ErrorRise = %v, %v, want 5 points: 6.0%% less 1.0%%", got, ok)
	}
}

func TestErrorsRegressedOnlyWhenTheRiseIsMoreThanTheThreshold(t *testing.T) {
	c := compareRuns(t, withOutcomes(1000, 5, 5), withOutcomes(500, 20, 10)) // +5 points

	for _, tc := range []struct {
		threshold float64
		want      bool
	}{{4.9, true}, {5, false}, {6, false}, {0, true}} {
		if got := c.ErrorsRegressed(tc.threshold); got != tc.want {
			t.Errorf("a rise of 5 points against a threshold of %v: regressed = %v, want %v", tc.threshold, got, tc.want)
		}
	}

	same := compareRuns(t, withOutcomes(1000, 5, 5), withOutcomes(1000, 5, 5))
	better := compareRuns(t, withOutcomes(1000, 50, 50), withOutcomes(1000, 0, 0))
	for name, c := range map[string]Comparison{"no change": same, "fewer errors": better} {
		if c.ErrorsRegressed(0) {
			t.Errorf("%s: regressed against a threshold of 0, want not: only a rise counts", name)
		}
	}
}

func TestErrorRiseIsUnknownWhenARunSentNothing(t *testing.T) {
	for name, c := range map[string]Comparison{
		"A sent nothing": compareRuns(t, withOutcomes(0, 0, 0), withOutcomes(100, 0, 0)),
		"B sent nothing": compareRuns(t, withOutcomes(100, 0, 0), withOutcomes(0, 0, 0)),
	} {
		if got, ok := c.ErrorRise(); ok {
			t.Errorf("%s: ErrorRise = %v, want unknown", name, got)
		}
		if c.ErrorsRegressed(0) {
			t.Errorf("%s: regressed although there is no rate to compare", name)
		}
	}
}

func TestDiffTableShowsTheErrorRateOfBothRuns(t *testing.T) {
	var buf strings.Builder
	if err := compareRuns(t, withOutcomes(1000, 5, 5), withOutcomes(500, 20, 10)).WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, "error rate") {
			line = l
		}
	}
	if got := strings.Join(strings.Fields(line), " "); got != "error rate 1.0% 6.0% +5.0 points" {
		t.Errorf("error rate row = %q, want %q", got, "error rate 1.0% 6.0% +5.0 points")
	}
}

func TestErrorPercentsAreTheRatesOfBothRuns(t *testing.T) {
	c := compareRuns(t, withOutcomes(1000, 5, 5), withOutcomes(500, 20, 10))

	a, b, ok := c.ErrorPercents()
	if !ok || math.Abs(a-1) > 1e-9 || math.Abs(b-6) > 1e-9 {
		t.Errorf("ErrorPercents = %v, %v, %v, want 1, 6, true", a, b, ok)
	}
	if _, _, ok := compareRuns(t, withOutcomes(0, 0, 0), withOutcomes(10, 0, 0)).ErrorPercents(); ok {
		t.Error("ErrorPercents is known although A sent nothing")
	}
}
