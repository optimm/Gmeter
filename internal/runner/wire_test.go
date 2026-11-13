package runner

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// A generator on another machine sends its Result as JSON; the controller must
// get back exactly what was sent, distributions included, or the merged
// percentiles would be wrong.
func TestAResultSurvivesJSONExactly(t *testing.T) {
	want := generatorResult(98, 10*time.Millisecond, 1, "GET /z", 3*time.Second)
	want.Labels = append(want.Labels, LabelResult{Label: "GET /b", Corrected: histOf(), Service: histOf()}) // no phases, no wait
	want.Timeline = append(want.Timeline, Window{End: 1500 * time.Millisecond, Interval: 500 * time.Millisecond, Partial: true})

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got *Result
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the result changed on its way through JSON:\n got %+v\nwant %+v", got, want)
	}
	if got.Corrected.Quantile(0.99) != want.Corrected.Quantile(0.99) || got.Timeline[0].Latency.Count() != 98 {
		t.Errorf("distributions did not survive: p99 %v, window 1 has %d samples", got.Corrected.Quantile(0.99), got.Timeline[0].Latency.Count())
	}
	if got.Timeline[1].Latency != nil || got.Labels[1].Wait != nil || got.Labels[1].Phases != nil {
		t.Error("what was absent came back present")
	}
}
