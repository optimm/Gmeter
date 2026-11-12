package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
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
