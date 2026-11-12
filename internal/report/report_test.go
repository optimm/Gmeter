package report

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

// sampleResult is a 10s run: 105 scheduled, 100 responses (90 ok, 10 HTTP 503),
// 5 timeouts, nothing dropped. Latencies are 1ms..100ms, one sample each.
func sampleResult() *runner.Result {
	corrected, service, lag := hist.New(), hist.New(), hist.New()
	for i := 1; i <= 100; i++ {
		corrected.Record(time.Duration(i) * time.Millisecond)
		service.Record(time.Duration(i) * time.Microsecond * 100)
		lag.Record(time.Duration(i) * time.Microsecond) // healthy: 1..100µs
	}
	return &runner.Result{
		Elapsed:   10 * time.Second,
		Scheduled: 105,
		Late:      3,
		Statuses:  map[int]uint64{200: 90, 503: 10},
		Errors:    map[string]uint64{"timeout": 5},
		Corrected: corrected,
		Service:   service,
		TimerLag:  lag,
	}
}

func TestSummarizeDerivesCountsAndRates(t *testing.T) {
	s := Summarize(sampleResult(), 10)

	if s.Scheduled != 105 || s.Responses != 100 || s.OK != 90 || s.Failed != 5 || s.Dropped != 0 {
		t.Fatalf("counts wrong: %+v", s)
	}
	if s.TargetRPS != 10 {
		t.Fatalf("TargetRPS = %v, want 10", s.TargetRPS)
	}
	if s.AchievedRPS != 10 { // 100 responses / 10s
		t.Fatalf("AchievedRPS = %v, want 10", s.AchievedRPS)
	}
	if s.ElapsedSeconds != 10 {
		t.Fatalf("ElapsedSeconds = %v, want 10", s.ElapsedSeconds)
	}
}

func TestSummarizeExpressesLatencyInMilliseconds(t *testing.T) {
	s := Summarize(sampleResult(), 10)

	c := s.Corrected
	if c.Count != 100 || c.MinMs != 1 || c.MaxMs != 100 {
		t.Fatalf("corrected count/min/max = %d/%v/%v, want 100/1/100", c.Count, c.MinMs, c.MaxMs)
	}
	if c.P50Ms < 50 || c.P50Ms > 50.2 {
		t.Fatalf("corrected P50Ms = %v, want ~50", c.P50Ms)
	}
	if c.P99Ms < 99 || c.P99Ms > 99.2 {
		t.Fatalf("corrected P99Ms = %v, want ~99", c.P99Ms)
	}
	if s.Service.MaxMs != 10 { // 100 * 100µs
		t.Fatalf("service MaxMs = %v, want 10", s.Service.MaxMs)
	}
}

func TestSummaryJSONRoundTrips(t *testing.T) {
	want := Summarize(sampleResult(), 10)

	var buf bytes.Buffer
	if err := want.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got Summary
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got.Scheduled != want.Scheduled || got.Corrected.P99Ms != want.Corrected.P99Ms ||
		got.Statuses[503] != 10 || got.Errors["timeout"] != 5 {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, want)
	}
}

func TestTextReportShowsTheNumbersThatMatter(t *testing.T) {
	var buf bytes.Buffer
	Summarize(sampleResult(), 10).WriteText(&buf)
	out := buf.String()

	for _, want := range []string{
		"105 scheduled", "90 ok", "5 failed",
		"achieved", "10.0 rps", "target 10",
		"503: 10", "timeout: 5",
		"corrected", "service",
		"p50", "p99", "p99.9", "max",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report is missing %q:\n%s", want, out)
		}
	}
}

func TestTextReportOmitsEmptySections(t *testing.T) {
	res := sampleResult()
	res.Errors = map[string]uint64{}
	var buf bytes.Buffer
	Summarize(res, 10).WriteText(&buf)
	if strings.Contains(buf.String(), "errors") {
		t.Fatalf("an error-free run should not print an errors line:\n%s", buf.String())
	}
}

func TestFormatDurationPicksAReadableUnit(t *testing.T) {
	cases := map[time.Duration]string{
		0:                        "0ns",
		850 * time.Nanosecond:    "850ns",
		42 * time.Microsecond:    "42µs",
		4_100 * time.Microsecond: "4.1ms",
		41 * time.Millisecond:    "41.0ms",
		1900 * time.Millisecond:  "1.90s",
		75 * time.Second:         "75.00s",
	}
	for d, want := range cases {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestSummarizeReportsGeneratorHealth(t *testing.T) {
	s := Summarize(sampleResult(), 10)

	g := s.Generator
	if g.TimerLagP99Ms < 0.099 || g.TimerLagP99Ms > 0.1 {
		t.Fatalf("TimerLagP99Ms = %v, want ~0.099 (99µs)", g.TimerLagP99Ms)
	}
	if g.TimerLagMaxMs != 0.1 {
		t.Fatalf("TimerLagMaxMs = %v, want 0.1 (100µs)", g.TimerLagMaxMs)
	}
	if g.Late != 3 {
		t.Fatalf("Late = %d, want 3", g.Late)
	}
	if len(s.Warnings) != 0 {
		t.Fatalf("a healthy run should have no warnings, got %v", s.Warnings)
	}
}

func TestWarnsWhenTheGeneratorCannotKeepTime(t *testing.T) {
	res := sampleResult()
	res.TimerLag = hist.New()
	for range 100 {
		res.TimerLag.Record(20 * time.Millisecond) // timers waking 20ms late
	}

	s := Summarize(res, 10)

	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "timer lag") {
		t.Fatalf("want exactly one timer-lag warning, got %v", s.Warnings)
	}
}

func TestWarnsWhenManyRequestsWereLate(t *testing.T) {
	res := sampleResult()
	res.Late = 30 // 30 of 105 scheduled (~29%)

	s := Summarize(res, 10)

	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "-workers") {
		t.Fatalf("want exactly one saturation warning mentioning -workers, got %v", s.Warnings)
	}
}

func TestTextReportShowsGeneratorLineAndWarnings(t *testing.T) {
	var healthy bytes.Buffer
	Summarize(sampleResult(), 10).WriteText(&healthy)
	for _, want := range []string{"generator", "timer lag p99", "late 3"} {
		if !strings.Contains(healthy.String(), want) {
			t.Errorf("healthy report is missing %q:\n%s", want, healthy.String())
		}
	}
	if strings.Contains(healthy.String(), "warning") {
		t.Errorf("healthy report must not print a warning:\n%s", healthy.String())
	}

	late := sampleResult()
	late.Late = 30
	var sick bytes.Buffer
	Summarize(late, 10).WriteText(&sick)
	if !strings.Contains(sick.String(), "warning") || !strings.Contains(sick.String(), "-workers") {
		t.Errorf("a saturated run must print the warning:\n%s", sick.String())
	}
}

func TestJSONIncludesGeneratorHealthAndAlwaysHasAWarningsArray(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(sampleResult(), 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	gen, ok := raw["generator"].(map[string]any)
	if !ok || gen["late_dispatches"] != float64(3) {
		t.Fatalf("generator section missing or wrong: %v", raw["generator"])
	}
	// [] rather than null, so consumers (and diffs) can rely on the key being a list.
	if w, ok := raw["warnings"].([]any); !ok || len(w) != 0 {
		t.Fatalf("warnings should be an empty array, got %#v", raw["warnings"])
	}
}

// windows builds n one-second windows (200 rps, 5ms p99), with a freeze at index spike.
func windows(n, spike int) []runner.Window {
	ws := make([]runner.Window, n)
	for i := range ws {
		w := runner.Window{
			End: time.Duration(i+1) * time.Second, Interval: time.Second,
			Scheduled: 200, Responses: 200, RPS: 200,
			P50: time.Millisecond, P99: 5 * time.Millisecond, Max: 9 * time.Millisecond,
		}
		if i == spike {
			w.Responses, w.RPS, w.P99, w.Max = 100, 100, 996*time.Millisecond, time.Second
		}
		ws[i] = w
	}
	return ws
}

func TestSummarizeCarriesTheVersionAndTheTimeline(t *testing.T) {
	res := sampleResult()
	res.Timeline = windows(3, 1)
	res.Timeline[2].Partial = true

	s := Summarize(res, 10)

	if s.Version != SchemaVersion {
		t.Fatalf("Version = %d, want %d", s.Version, SchemaVersion)
	}
	if len(s.Timeline) != 3 {
		t.Fatalf("Timeline has %d points, want 3", len(s.Timeline))
	}
	p := s.Timeline[1]
	if p.EndSeconds != 2 || p.P99Ms != 996 || p.MaxMs != 1000 || p.RPS != 100 || p.Responses != 100 {
		t.Fatalf("point 1 converted wrong: %+v", p)
	}
	if !s.Timeline[2].Partial || s.Timeline[0].Partial {
		t.Fatalf("Partial flags wrong: %+v", s.Timeline)
	}
}

func TestEmptyTimelineEncodesAsAnEmptyArray(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(sampleResult(), 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if tl, ok := raw["timeline"].([]any); !ok || len(tl) != 0 {
		t.Fatalf("timeline should be [], got %#v", raw["timeline"])
	}
}

// The reason full histograms are in the file: the saved distribution must
// rebuild to the same percentiles the run reported.
func TestSavedHistogramsRebuildTheSameDistribution(t *testing.T) {
	res := sampleResult()
	var buf bytes.Buffer
	if err := Summarize(res, 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}

	loaded, err := ReadSummary(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := hist.FromDump(loaded.Histograms.Corrected)
	if err != nil {
		t.Fatal(err)
	}

	if got.Count() != res.Corrected.Count() {
		t.Fatalf("Count = %d, want %d", got.Count(), res.Corrected.Count())
	}
	for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
		if got.Quantile(q) != res.Corrected.Quantile(q) {
			t.Errorf("Quantile(%v) = %v, want %v", q, got.Quantile(q), res.Corrected.Quantile(q))
		}
	}
}

func TestReadSummaryRefusesFilesItCannotTrust(t *testing.T) {
	cases := map[string]struct{ input, want string }{
		"not json":          {`this is not json`, "not a gmeter summary"},
		"no version":        {`{"scheduled": 5}`, "version"},
		"future version":    {`{"version": 99}`, "version"},
		"corrupt histogram": {`{"version": 1, "histograms": {"corrected": {"precision_bits": 3}}}`, "precision"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ReadSummary(strings.NewReader(tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestSparklineScalesFromZeroToTheTallestBar(t *testing.T) {
	if got := sparkline([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 80, math.Max); got != "▁▂▃▄▅▆▇█" {
		t.Errorf("ramp = %q, want ▁▂▃▄▅▆▇█", got)
	}
	if got := sparkline([]float64{0, 0, 0}, 80, math.Max); got != "▁▁▁" {
		t.Errorf("all zeros = %q, want ▁▁▁", got)
	}
	if got := sparkline(nil, 80, math.Max); got != "" {
		t.Errorf("empty input = %q, want empty", got)
	}
}

func TestSparklineKeepsSpikesAndDipsWhenItHasToShrink(t *testing.T) {
	vals := make([]float64, 160)
	for i := range vals {
		vals[i] = 1
	}

	spike := append([]float64(nil), vals...)
	spike[77] = 100
	if got := sparkline(spike, 80, math.Max); len([]rune(got)) != 80 || !strings.Contains(got, "█") {
		t.Errorf("a spike must survive compression to 80 columns: %q", got)
	}

	dip := append([]float64(nil), vals...)
	dip[77] = 0
	if got := sparkline(dip, 80, math.Min); len([]rune(got)) != 80 || !strings.Contains(got, "▁") {
		t.Errorf("a dip must survive compression to 80 columns: %q", got)
	}
}

func TestTextReportPlotsTheTimelineAndNamesTheWorstWindow(t *testing.T) {
	res := sampleResult()
	res.Timeline = windows(12, 5) // the freeze ends at 6.0s
	var buf bytes.Buffer
	Summarize(res, 10).WriteText(&buf)
	out := buf.String()

	for _, want := range []string{"timeline", "p99 ", "rps ", "█", "▁", "worst 996", "6.0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

func TestTextReportSkipsTheTimelineForRunsTooShortToPlot(t *testing.T) {
	res := sampleResult()
	res.Timeline = windows(1, -1)
	var buf bytes.Buffer
	Summarize(res, 10).WriteText(&buf)
	if strings.Contains(buf.String(), "timeline") {
		t.Fatalf("a single window is not a timeline:\n%s", buf.String())
	}
}

// stagedResult is a two-step run: 5s at 10 rps with every response at 2ms,
// then 10s at 15 rps with every response at 80ms (the server fell over at the
// step). The run's totals are the sum of the stages.
func stagedResult() *runner.Result {
	stage := func(st pacer.Stage, n int, lat time.Duration, failed uint64) runner.StageResult {
		c, sv := hist.New(), hist.New()
		for range n {
			c.Record(lat)
			sv.Record(lat)
		}
		return runner.StageResult{Stage: st, Scheduled: uint64(n) + failed, Failed: failed, Corrected: c, Service: sv}
	}
	s1 := stage(pacer.Stage{Duration: 5 * time.Second, From: 10, To: 10}, 50, 2*time.Millisecond, 0)
	s2 := stage(pacer.Stage{Duration: 10 * time.Second, From: 15, To: 15}, 148, 80*time.Millisecond, 2)

	corrected, service, lag := hist.New(), hist.New(), hist.New()
	for _, st := range []runner.StageResult{s1, s2} {
		corrected.Merge(st.Corrected)
		service.Merge(st.Service)
	}
	return &runner.Result{
		Elapsed: 15 * time.Second, Scheduled: s1.Scheduled + s2.Scheduled,
		Statuses: map[int]uint64{200: 198}, Errors: map[string]uint64{"timeout": 2},
		Corrected: corrected, Service: service, TimerLag: lag,
		Stages: []runner.StageResult{s1, s2},
	}
}

func TestSummarizeBreaksTheRunDownByStage(t *testing.T) {
	s := Summarize(stagedResult(), 13.33)
	if len(s.Stages) != 2 {
		t.Fatalf("%d stages, want 2", len(s.Stages))
	}
	a, b := s.Stages[0], s.Stages[1]
	if a.Profile != "5s@10" || b.Profile != "10s@15" {
		t.Errorf("profiles %q and %q, want 5s@10 and 10s@15", a.Profile, b.Profile)
	}
	if a.DurationSeconds != 5 || b.DurationSeconds != 10 || a.TargetRPS != 10 || b.TargetRPS != 15 {
		t.Errorf("durations/targets = %v %v / %v %v, want 5 10 / 10 15", a.DurationSeconds, b.DurationSeconds, a.TargetRPS, b.TargetRPS)
	}
	if a.Scheduled != 50 || b.Scheduled != 150 || b.Failed != 2 {
		t.Errorf("scheduled/failed = %d %d / %d, want 50 150 / 2", a.Scheduled, b.Scheduled, b.Failed)
	}
	if a.Corrected.P99Ms < 1.9 || a.Corrected.P99Ms > 2.1 {
		t.Errorf("stage 1 p99 = %vms, want about 2", a.Corrected.P99Ms)
	}
	if b.Corrected.P50Ms < 79 || b.Corrected.P50Ms > 81 || b.Service.P50Ms < 79 || b.Service.P50Ms > 81 {
		t.Errorf("stage 2 p50 corrected/service = %v/%vms, want about 80", b.Corrected.P50Ms, b.Service.P50Ms)
	}
}

func TestTextReportShowsAStageTableForSeveralStagesOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(stagedResult(), 13.33).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"stages", "5s@10", "10s@15", "on average"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}

	// A constant rate is one stage: a table with one row would only repeat the
	// run's own numbers.
	one := sampleResult()
	one.Stages = stagedResult().Stages[:1]
	buf.Reset()
	if err := Summarize(one, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "stages") || strings.Contains(buf.String(), "on average") {
		t.Errorf("a single-stage run should not print a stage table:\n%s", buf.String())
	}
}

func TestJSONAlwaysHasAStagesArray(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(sampleResult(), 10).WriteJSON(&buf); err != nil { // no stages recorded
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"stages": []`) {
		t.Errorf("stages should encode as [] and not null:\n%s", buf.String())
	}
}

func TestAnAverageTargetRateIsRoundedForReading(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(stagedResult(), 2000.0/3).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "target 666.667 on average") {
		t.Errorf("want the target shown as 666.667, got:\n%s", out)
	}

	buf.Reset() // an ordinary constant rate still prints as written
	if err := Summarize(sampleResult(), 200).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "(target 200)") {
		t.Errorf("a constant rate of 200 should print as 200, got:\n%s", out)
	}
}
