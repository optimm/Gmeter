package report

import (
	"strings"
	"testing"
)

// endpointsOf is n endpoints named /e0../e(n-1), each with 500 requests and the
// given p99; withRise makes the first rose of them take factor times as long.
func endpointsOf(n int, p99 float64) []EndpointSummary {
	out := make([]EndpointSummary, n)
	for i := range out {
		out[i] = ep("GET /e"+string(rune('0'+i)), 500, 5, p99)
	}
	return out
}

func withRise(eps []EndpointSummary, rose int, factor float64) []EndpointSummary {
	out := append([]EndpointSummary(nil), eps...)
	for i := range out[:rose] {
		out[i].Corrected.P99Ms *= factor
	}
	return out
}

func TestSharedRiseNeedsMostEndpointsToRiseTogether(t *testing.T) {
	a := endpointsOf(4, 10)
	for _, tc := range []struct {
		rose int
		want bool
	}{{0, false}, {1, false}, {2, false}, {3, true}, {4, true}} { // of 4 judged
		c := compared(t, a, withRise(a, tc.rose, 3))

		rose, judged, ok := c.SharedRise()

		if rose != tc.rose || judged != 4 || ok != tc.want {
			t.Errorf("%d of 4 up threefold: SharedRise = %d, %d, %v, want %d, 4, %v", tc.rose, rose, judged, ok, tc.rose, tc.want)
		}
	}
}

func TestSharedRiseNeedsAtLeastThreeEndpoints(t *testing.T) {
	a := endpointsOf(2, 10)
	if _, _, ok := compared(t, a, withRise(a, 2, 3)).SharedRise(); ok {
		t.Error("both of 2 endpoints rose, which is not enough endpoints to say they rose together")
	}
	three := endpointsOf(3, 10)
	if rose, judged, ok := compared(t, three, withRise(three, 3, 3)).SharedRise(); !ok || rose != 3 || judged != 3 {
		t.Errorf("all of 3 rose: SharedRise = %d, %d, %v, want 3, 3, true", rose, judged, ok)
	}
}

func TestSharedRiseCountsOnlyARiseOfMoreThanFiftyPercent(t *testing.T) {
	a := endpointsOf(3, 10)
	if _, _, ok := compared(t, a, withRise(a, 3, 1.5)).SharedRise(); ok {
		t.Error("a rise of exactly 50% counted: it takes more than that")
	}
	if _, _, ok := compared(t, a, withRise(a, 3, 1.51)).SharedRise(); !ok {
		t.Error("a rise of 51% did not count")
	}
	if _, _, ok := compared(t, a, withRise(a, 3, 0.2)).SharedRise(); ok {
		t.Error("endpoints that got faster counted as rising")
	}
}

// An endpoint whose p99 cannot be judged (fewer than 100 samples) is neither
// counted as having risen nor as an endpoint that did not.
func TestSharedRiseLeavesOutEndpointsTooSmallToJudge(t *testing.T) {
	a := endpointsOf(5, 10)
	a[3], a[4] = ep("GET /small3", 10, 5, 10), ep("GET /small4", 10, 5, 10)
	b := withRise(a, 3, 3)

	rose, judged, ok := compared(t, a, b).SharedRise()

	if rose != 3 || judged != 3 || !ok {
		t.Errorf("SharedRise = %d, %d, %v, want 3 of the 3 endpoints big enough to judge", rose, judged, ok)
	}
}

func TestDiffNotesWhenMostEndpointsRoseTogether(t *testing.T) {
	a := endpointsOf(4, 10)
	c := compared(t, a, withRise(a, 3, 3)) // 3 of 4: enough, and the two numbers differ

	var notes []string
	for _, n := range c.Notes {
		if strings.Contains(n, "shared cause") {
			notes = append(notes, n)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("%d notes about a shared cause in %q, want 1", len(notes), c.Notes)
	}
	for _, want := range []string{"3 of 4 judged endpoints", "more than 50%", "than 3 separate regressions", "cannot say which endpoint"} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("the note %q does not say %q", notes[0], want)
		}
	}

	var buf strings.Builder
	if err := c.WriteText(&buf, "a.json", "b.json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "note  3 of 4 judged endpoints") {
		t.Errorf("the text report does not show the note:\n%s", buf.String())
	}
}

func TestDiffHasNoSharedCauseNoteWhenOnlyOneEndpointRose(t *testing.T) {
	a := endpointsOf(4, 10)
	for _, n := range compared(t, a, withRise(a, 1, 10)).Notes {
		if strings.Contains(n, "shared cause") {
			t.Errorf("a note about a shared cause although one endpoint rose: %q", n)
		}
	}
}

// An endpoint with a zero p99 in A has no relative change, so it says nothing
// about whether endpoints rose together.
func TestSharedRiseLeavesOutEndpointsWithNoBaseline(t *testing.T) {
	a := endpointsOf(4, 10)
	a[3] = ep("GET /nobaseline", 500, 0, 0)
	b := withRise(a, 3, 3)
	b[3].Corrected.P99Ms = 40

	rose, judged, ok := compared(t, a, b).SharedRise()

	if rose != 3 || judged != 3 || !ok {
		t.Errorf("SharedRise = %d, %d, %v, want 3 of the 3 endpoints with a baseline", rose, judged, ok)
	}
}
