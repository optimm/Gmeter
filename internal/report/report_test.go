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
	corrected, service := hist.New(), hist.New()
	for i := 1; i <= 100; i++ {
		corrected.Record(time.Duration(i) * time.Millisecond)
		service.Record(time.Duration(i) * time.Microsecond * 100)
	}
	return &runner.Result{
		Elapsed:   10 * time.Second,
		Scheduled: 105,
		Statuses:  map[int]uint64{200: 90, 503: 10},
		Errors:    map[string]uint64{"timeout": 5},
		Corrected: corrected,
		Service:   service,
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
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
