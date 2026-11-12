package find

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var info = RunInfo{URL: "http://x/", Method: "GET", Workers: 200, TrialSeconds: 10, WarmupSeconds: 2, Arrivals: "poisson", Seed: 7}

func obs(rate float64, p99 time.Duration, errs float64, reasons ...string) Observation {
	return Observation{Rate: rate, Scheduled: uint64(rate * 10), Observed: p99, ErrorRate: errs, Reasons: reasons}
}

// found is a typical search: capacity between 700 and 750, confirmed twice.
func found() Outcome {
	return Outcome{
		Trials: []Trial{
			{Phase: Grow, Rate: 400, Verdict: Pass, Detail: obs(400, 6*time.Millisecond, 0)},
			{Phase: Grow, Rate: 800, Verdict: Fail, Detail: obs(800, 96*time.Millisecond, 0, "p99 is 96.0ms, over the 50ms target")},
			{Phase: Bisect, Rate: 700, Verdict: Pass, Detail: obs(700, 31*time.Millisecond, 0.002)},
			{Phase: Bisect, Rate: 750, Verdict: Fail, Detail: obs(750, 71*time.Millisecond, 0, "p99 is 71.0ms, over the 50ms target")},
			{Phase: Confirm, Rate: 700, Verdict: Pass, Detail: obs(700, 33*time.Millisecond, 0)},
			{Phase: Confirm, Rate: 700, Verdict: Pass, Detail: obs(700, 29*time.Millisecond, 0)},
		},
		Best: 700, NextFail: 750, Confirmed: 2, ConfirmRuns: 2,
	}
}

var slo99 = SLO{Quantile: 0.99, Latency: 50 * time.Millisecond, MaxErrorRate: 0.01}

func text(t *testing.T, o Outcome, interrupted bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := NewSummary(o, info, slo99, interrupted).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestTheTextReportListsEveryTrialAndStatesTheAnswer(t *testing.T) {
	out := text(t, found(), false)
	for _, want := range []string{
		"p99 ≤ 50ms", "poisson", "seed 7", // what was asked
		"grow", "bisect", "confirm", "pass", "FAIL", "96.0ms", "0.2%", // the trials
		"about 700 rps", "750 rps", "2 of 2", // the answer and its bracket and confirmations
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"unstable", "at least", "generator", "interrupted"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a clean result should not mention %q:\n%s", unwanted, out)
		}
	}
}

func TestTheReportSaysSoWhenNothingMeetsTheTarget(t *testing.T) {
	o := Outcome{
		Trials:   []Trial{{Phase: Grow, Rate: 100, Verdict: Fail, Detail: obs(100, time.Second, 0, "p99 is 1.00s, over the 50ms target")}},
		NextFail: 1,
	}
	if out := text(t, o, false); !strings.Contains(out, "no rate") || !strings.Contains(out, "1 rps") {
		t.Errorf("want a statement that no rate down to 1 rps met the target:\n%s", out)
	}
}

func TestTheReportCallsTheAnswerALowerBoundWhenTheSearchHitsItsMaximum(t *testing.T) {
	o := Outcome{Trials: []Trial{{Phase: Grow, Rate: 1000, Verdict: Pass, Detail: obs(1000, time.Millisecond, 0)}}, Best: 1000, LowerBound: true}
	if out := text(t, o, false); !strings.Contains(out, "at least 1000 rps") || !strings.Contains(out, "-max") {
		t.Errorf("want 'at least 1000 rps' and a pointer to -max:\n%s", out)
	}
}

func TestTheReportWarnsWhenTheGeneratorMayBeTheLimit(t *testing.T) {
	o := found()
	o.GeneratorLimited = true
	o.Trials[3].Verdict = Limited
	o.Trials[3].Detail = obs(750, 20*time.Millisecond, 0, "generator timer lag p99 is 9.0ms, so this result may be the generator's and not the server's")
	out := text(t, o, false)
	if !strings.Contains(out, "load generator") || !strings.Contains(out, "may sustain more") {
		t.Errorf("want a warning that the server may sustain more than the generator could show:\n%s", out)
	}
	if !strings.Contains(out, "limited") {
		t.Errorf("the limited trial should be marked as such:\n%s", out)
	}
}

func TestTheReportFlagsAnAnswerThatDidNotHoldWhenRepeated(t *testing.T) {
	o := found()
	o.Confirmed = 1
	o.Trials[5].Verdict = Fail
	o.Trials[5].Detail = obs(700, 80*time.Millisecond, 0, "p99 is 80.0ms, over the 50ms target")
	out := text(t, o, false)
	if !strings.Contains(out, "unstable") || !strings.Contains(out, "1 of 2") {
		t.Errorf("want the answer flagged unstable, 1 of 2 confirmations:\n%s", out)
	}
}

func TestTheReportSaysWhenTheSearchWasInterrupted(t *testing.T) {
	o := Outcome{Trials: found().Trials[:2], Best: 400}
	if out := text(t, o, true); !strings.Contains(out, "interrupted") || !strings.Contains(out, "400 rps") {
		t.Errorf("want the interruption and the best rate so far:\n%s", out)
	}
}

func TestTheJSONRoundTripsAndAlwaysHasATrialsArray(t *testing.T) {
	var buf bytes.Buffer
	if err := NewSummary(found(), info, slo99, false).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version int `json:"version"`
		Run     struct {
			URL      string `json:"url"`
			Arrivals string `json:"arrivals"`
			Seed     uint64 `json:"seed"`
		} `json:"run"`
		Target struct {
			Quantile     float64 `json:"quantile"`
			LatencyMs    float64 `json:"latency_ms"`
			MaxErrorRate float64 `json:"max_error_rate"`
		} `json:"target"`
		Result struct {
			Found       bool    `json:"found"`
			MaxRPS      float64 `json:"max_rps"`
			NextFailRPS float64 `json:"next_fail_rps"`
			Confirmed   int     `json:"confirmed"`
			ConfirmRuns int     `json:"confirm_runs"`
		} `json:"result"`
		Trials []struct {
			Phase      string   `json:"phase"`
			RPS        float64  `json:"rps"`
			Verdict    string   `json:"verdict"`
			ObservedMs float64  `json:"observed_ms"`
			Reasons    []string `json:"reasons"`
		} `json:"trials"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if got.Version != 1 || got.Run.URL != "http://x/" || got.Run.Arrivals != "poisson" || got.Run.Seed != 7 {
		t.Errorf("run info wrong: %+v", got.Run)
	}
	if got.Target.Quantile != 0.99 || got.Target.LatencyMs != 50 || got.Target.MaxErrorRate != 0.01 {
		t.Errorf("target wrong: %+v", got.Target)
	}
	if !got.Result.Found || got.Result.MaxRPS != 700 || got.Result.NextFailRPS != 750 || got.Result.Confirmed != 2 || got.Result.ConfirmRuns != 2 {
		t.Errorf("result wrong: %+v", got.Result)
	}
	if len(got.Trials) != 6 || got.Trials[1].Verdict != "fail" || got.Trials[0].Verdict != "pass" || got.Trials[1].ObservedMs != 96 {
		t.Errorf("trials wrong: %+v", got.Trials)
	}
	if len(got.Trials[1].Reasons) != 1 {
		t.Errorf("a failed trial should carry its reason: %+v", got.Trials[1])
	}

	buf.Reset()
	if err := NewSummary(Outcome{}, info, slo99, false).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"trials": []`) {
		t.Errorf("no trials should encode as [] and not null:\n%s", buf.String())
	}
}
