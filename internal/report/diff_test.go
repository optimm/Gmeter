package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/runner"
)

// mkSummary is a 100-request run in which slowCount requests took `slow` and
// the rest took 2ms, so its p99 is `slow` whenever slowCount >= 2.
func mkSummary(slow time.Duration, slowCount int) Summary {
	corrected := hist.New()
	for range 100 - slowCount {
		corrected.Record(2 * time.Millisecond)
	}
	for range slowCount {
		corrected.Record(slow)
	}
	res := &runner.Result{
		Elapsed: time.Second, Scheduled: 100,
		Statuses: map[int]uint64{200: 100}, Errors: map[string]uint64{},
		Corrected: corrected, Service: corrected, TimerLag: hist.New(),
	}
	s := Summarize(res, 100)
	s.Run = RunInfo{URL: "http://x/", Method: "GET", Workers: 10, DurationSeconds: 1}
	return s
}

func rowByLabel(t *testing.T, rows []Row, label string) Row {
	t.Helper()
	for _, r := range rows {
		if r.Label == label {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", label, rows)
	return Row{}
}

func TestCompareReportsTheRelativeChangeOfEachPercentile(t *testing.T) {
	c, err := Compare(mkSummary(100*time.Millisecond, 2), mkSummary(150*time.Millisecond, 2))
	if err != nil {
		t.Fatal(err)
	}

	if c.P99ChangePct < 49.5 || c.P99ChangePct > 50.5 {
		t.Fatalf("P99ChangePct = %v, want ~+50%% (100ms -> 150ms)", c.P99ChangePct)
	}
	if got := rowByLabel(t, c.Latency, "p99").Change; !strings.HasPrefix(got, "+49.") && !strings.HasPrefix(got, "+50.") {
		t.Errorf("p99 change shown as %q, want about +50%%", got)
	}
	if got := rowByLabel(t, c.Latency, "p50").Change; got != "+0.0%" {
		t.Errorf("p50 did not move (2ms both) but is shown as %q", got)
	}
}

func TestRegressedGatesOnTheP99Threshold(t *testing.T) {
	worse, _ := Compare(mkSummary(100*time.Millisecond, 2), mkSummary(150*time.Millisecond, 2))
	if !worse.Regressed(20) {
		t.Error("+50% p99 must count as a regression at a 20% threshold")
	}
	if worse.Regressed(60) {
		t.Error("+50% p99 is within a 60% threshold")
	}

	better, _ := Compare(mkSummary(150*time.Millisecond, 2), mkSummary(100*time.Millisecond, 2))
	if better.Regressed(0) {
		t.Error("a faster run is never a regression, even at a 0% threshold")
	}
}

// Percentiles alone cannot say how many requests got slower. The saved
// histograms can: what share of B is slower than A's p99?
func TestCompareShowsTheShareOfBSlowerThanAsPercentiles(t *testing.T) {
	a := mkSummary(100*time.Millisecond, 2)  // p99 = 100ms
	b := mkSummary(500*time.Millisecond, 10) // 10% of requests at 500ms

	c, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}

	var p99 Shift
	for _, s := range c.Shifts {
		if s.Quantile == 0.99 {
			p99 = s
		}
	}
	if p99.Value < 100*time.Millisecond || p99.Value > 101*time.Millisecond {
		t.Fatalf("shift value = %v, want A's p99 (~100ms)", p99.Value)
	}
	if p99.Share < 0.0999 || p99.Share > 0.1001 {
		t.Fatalf("share of B slower than A's p99 = %v, want 0.10", p99.Share)
	}

	// A run compared with itself can never have more than (1-q) of its requests
	// above its own q-th percentile.
	same, _ := Compare(a, a)
	for _, s := range same.Shifts {
		if s.Share > 1-s.Quantile+1e-9 {
			t.Errorf("comparing a run with itself: %v of requests are slower than its own p%v", s.Share, s.Quantile*100)
		}
	}
}

func TestCompareFlagsRunsThatAreNotLikeForLike(t *testing.T) {
	a := mkSummary(100*time.Millisecond, 2)
	if c, _ := Compare(a, a); len(c.Notes) != 0 {
		t.Fatalf("identical runs should have no notes, got %v", c.Notes)
	}

	b := mkSummary(100*time.Millisecond, 2)
	b.TargetRPS = 200
	b.Run.Workers = 20
	b.Run.URL = "http://other/"
	c, _ := Compare(a, b)
	notes := strings.Join(c.Notes, "\n")
	for _, want := range []string{"target rate", "workers", "URL"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes should mention %q:\n%s", want, notes)
		}
	}

	noisy := mkSummary(100*time.Millisecond, 2)
	noisy.Warnings = []string{"generator timer lag p99 is 20ms"}
	c, _ = Compare(a, noisy)
	if got := strings.Join(c.Notes, "\n"); !strings.Contains(got, "B") || !strings.Contains(got, "warning") {
		t.Errorf("a warning on run B must be called out:\n%s", got)
	}
}

// Spinning removes the timer's lateness from every request's latency, so a
// run that spun and one that did not differ in what they measure.
func TestCompareFlagsADifferentPacingSpin(t *testing.T) {
	a := mkSummary(100*time.Millisecond, 2)
	b := mkSummary(100*time.Millisecond, 2)
	b.Run.SpinSeconds = 0.0002

	c, _ := Compare(a, b)
	notes := strings.Join(c.Notes, "\n")
	for _, want := range []string{"spin", "off", "200µs"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes should mention %q:\n%s", want, notes)
		}
	}
	if same, _ := Compare(b, b); len(same.Notes) != 0 {
		t.Errorf("two runs with the same spin should have no notes, got %v", same.Notes)
	}
}

func notesOf(t *testing.T, a, b Summary) string {
	t.Helper()
	c, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(c.Notes, "\n")
}

// Evenly spaced and Poisson arrivals at the same average rate do not queue the
// same way, so a run of each is not a like-for-like comparison.
func TestCompareFlagsDifferentArrivalProcessesAndSeeds(t *testing.T) {
	even := mkSummary(100*time.Millisecond, 2)
	even.Run.Arrivals = "even"
	poisson := mkSummary(100*time.Millisecond, 2)
	poisson.Run.Arrivals, poisson.Run.Seed = "poisson", 7

	notes := notesOf(t, even, poisson)
	for _, want := range []string{"arrivals", "even", "poisson"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes should mention %q:\n%s", want, notes)
		}
	}

	// Files written before the setting existed were all evenly spaced.
	old := mkSummary(100*time.Millisecond, 2)
	if got := notesOf(t, old, even); got != "" {
		t.Errorf("an old file (no arrivals recorded) and an even run should not differ, got:\n%s", got)
	}

	// The same process with a different seed sees different bursts.
	other := poisson
	other.Run.Seed = 8
	if got := notesOf(t, poisson, other); !strings.Contains(got, "seed") {
		t.Errorf("poisson runs with different seeds should be noted:\n%s", got)
	}
	if got := notesOf(t, poisson, poisson); got != "" {
		t.Errorf("identical poisson runs should have no notes, got:\n%s", got)
	}
	// The seed means nothing for evenly spaced arrivals.
	evenOther := even
	evenOther.Run.Seed = 99
	if got := notesOf(t, even, evenOther); got != "" {
		t.Errorf("seeds of even runs should be ignored, got:\n%s", got)
	}
}

func TestCompareFlagsADifferentLoadProfileOnlyWhenTheSummariesDoNotAlreadySayso(t *testing.T) {
	step := mkSummary(100*time.Millisecond, 2) // 10s@100 then 10s@200: 20s, 150 rps on average
	step.Run.Profile, step.Run.DurationSeconds, step.TargetRPS = "10s@100,10s@200", 20, 150
	flat := mkSummary(100*time.Millisecond, 2) // 20s at 150 rps: the same duration and the same average
	flat.Run.Profile, flat.Run.DurationSeconds, flat.TargetRPS = "20s@150", 20, 150

	got := notesOf(t, step, flat)
	if !strings.Contains(got, "profile") || !strings.Contains(got, "10s@100,10s@200") || !strings.Contains(got, "20s@150") {
		t.Errorf("a step and a flat run with the same average should be told apart:\n%s", got)
	}

	// When the rate or the duration already differ, that is the note; the
	// profile would only say it twice.
	faster := flat
	faster.TargetRPS = 300
	faster.Run.Profile = "20s@300"
	if got := notesOf(t, flat, faster); strings.Contains(got, "profile") {
		t.Errorf("the rate difference is already noted, so the profile need not be:\n%s", got)
	}

	// A file from before profiles existed has none to compare.
	old := mkSummary(100*time.Millisecond, 2)
	old.TargetRPS, old.Run.DurationSeconds = 150, 20
	if got := notesOf(t, old, flat); strings.Contains(got, "profile") {
		t.Errorf("a file without a profile cannot differ in it, got:\n%s", got)
	}
}

func TestCompareRefusesAnUnusableHistogram(t *testing.T) {
	a := mkSummary(100*time.Millisecond, 2)
	b := mkSummary(100*time.Millisecond, 2)
	b.Histograms.Corrected.PrecisionBits = 3
	if _, err := Compare(a, b); err == nil {
		t.Fatal("Compare accepted a histogram it cannot rebuild")
	}
}

func TestComparisonTextNamesTheFilesAndShowsTheNumbers(t *testing.T) {
	c, err := Compare(mkSummary(100*time.Millisecond, 2), mkSummary(150*time.Millisecond, 2))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := c.WriteText(&buf, "before.json", "after.json"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{"before.json", "after.json", "p99", "+49.", "slower than A's p99", "achieved rps"} {
		if !strings.Contains(out, want) && !(want == "+49." && strings.Contains(out, "+50.")) {
			t.Errorf("diff output is missing %q:\n%s", want, out)
		}
	}
}
