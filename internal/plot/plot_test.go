package plot

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/results"
	"github.com/optimm/Gmeter/internal/runner"
)

// file writes samples as a results file and returns it for reading.
func file(t *testing.T, samples []runner.Sample) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := results.NewWriter(&buf)
	for _, s := range samples {
		w.Add(s)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

// second is 100 requests due 10ms apart, all answered 200: those due in the
// second starting at start take latency each, from when they were due and,
// because a worker was late to send them, a shorter one from when they were sent.
func second(start time.Duration, seq0 uint64, latency, late time.Duration) []runner.Sample {
	var out []runner.Sample
	for i := 0; i < 100; i++ {
		due := start + time.Duration(i)*10*time.Millisecond
		out = append(out, runner.Sample{Seq: seq0 + uint64(i), Due: due, Sent: due + late, Finished: due + latency, Status: 200})
	}
	return out
}

func TestSummarizeBucketsTheRunByDueTime(t *testing.T) {
	var rows []runner.Sample
	rows = append(rows, second(0, 0, 10*time.Millisecond, 0)...)
	rows = append(rows, second(time.Second, 100, 50*time.Millisecond, 40*time.Millisecond)...)

	d, err := Summarize(file(t, rows), 2)
	if err != nil {
		t.Fatal(err)
	}

	if d.Total != 200 || d.OK != 200 || d.Failed != 0 {
		t.Errorf("totals %d/%d/%d, want 200 requests, 200 ok, 0 failed", d.Total, d.OK, d.Failed)
	}
	if len(d.Buckets) != 2 {
		t.Fatalf("%d buckets, want 2: %+v", len(d.Buckets), d.Buckets)
	}
	first, secondB := d.Buckets[0], d.Buckets[1]
	if first.Due != 100 || secondB.Due != 100 {
		t.Errorf("due per bucket %d and %d, want 100 each", first.Due, secondB.Due)
	}
	near := func(got, want float64) bool { return got > want*0.99 && got < want*1.01 }
	if !near(first.Corrected.P50, 10) || !near(secondB.Corrected.P50, 50) {
		t.Errorf("corrected p50 %.2f and %.2f ms, want 10 and 50", first.Corrected.P50, secondB.Corrected.P50)
	}
	if !near(secondB.Service.P50, 10) { // sent 40ms late, took 10 from then
		t.Errorf("service p50 %.2f ms in the late second, want 10: the two clocks differ there", secondB.Service.P50)
	}
}

func TestSummarizeCountsFailuresAndTimesOnlyTimeoutsAmongThem(t *testing.T) {
	rows := []runner.Sample{
		{Seq: 0, Due: 0, Sent: 0, Finished: 5 * time.Millisecond, Status: 200},
		{Seq: 1, Due: 10 * time.Millisecond, Sent: 10 * time.Millisecond, Finished: 10 * time.Millisecond, Failure: "connection_refused"},
		{Seq: 2, Due: 20 * time.Millisecond, Sent: 20 * time.Millisecond, Finished: 1020 * time.Millisecond, Failure: "timeout"},
		{Seq: 3, Due: 30 * time.Millisecond, Sent: 30 * time.Millisecond, Finished: 35 * time.Millisecond, Status: 503},
		{Seq: 4, Due: 40 * time.Millisecond, Sent: 40 * time.Millisecond, Finished: 45 * time.Millisecond, Code: "Unavailable"},
	}

	d, err := Summarize(file(t, rows), 10)
	if err != nil {
		t.Fatal(err)
	}

	if d.Total != 5 || d.Failed != 2 || d.OK != 3 {
		t.Errorf("totals %d/%d/%d, want 5 requests, 3 answered, 2 failed", d.Total, d.OK, d.Failed)
	}
	if d.Failures["connection_refused"] != 1 || d.Failures["timeout"] != 1 {
		t.Errorf("failures %v", d.Failures)
	}
	if d.Statuses["200"] != 1 || d.Statuses["503"] != 1 || d.Statuses["Unavailable"] != 1 {
		t.Errorf("statuses %v", d.Statuses)
	}
	// The refused connection has no latency; the timeout's is the time it waited.
	if d.Corrected.Max < 1000 {
		t.Errorf("the slowest request took %.1f ms, want the timeout's 1000", d.Corrected.Max)
	}
	if d.Corrected.Count != 4 {
		t.Errorf("%d timed requests, want 4 (3 answered and the timeout)", d.Corrected.Count)
	}
}

func TestSummarizeOfOneInstantStillMakesAChart(t *testing.T) {
	d, err := Summarize(file(t, []runner.Sample{{Status: 200}}), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Buckets) != 1 || d.Total != 1 || d.BucketSeconds <= 0 {
		t.Errorf("got %+v", d)
	}
}

func TestSummarizeRefusesAFileWithNoRequests(t *testing.T) {
	if _, err := Summarize(file(t, nil), 10); err == nil || !strings.Contains(err.Error(), "no requests") {
		t.Errorf("err = %v, want one saying there are no requests", err)
	}
}

func TestThePageIsOneSelfContainedFile(t *testing.T) {
	d, err := Summarize(file(t, second(0, 0, 10*time.Millisecond, 0)), 10)
	if err != nil {
		t.Fatal(err)
	}
	d.Title = `a </script><b>run`

	var page bytes.Buffer
	if err := WriteHTML(&page, d); err != nil {
		t.Fatal(err)
	}
	html := page.String()

	for _, external := range []string{"https://", "http://", `src="`, `href="`} {
		if strings.Contains(html, external) {
			t.Errorf("the page refers to something outside itself: contains %q", external)
		}
	}
	if strings.Count(html, "</script>") != 1 {
		t.Errorf("the title closed the script: %d </script> in the page", strings.Count(html, "</script>"))
	}
	if !strings.Contains(html, `"total":100`) || !strings.Contains(html, "<title>") {
		t.Errorf("the page does not carry the data or a title")
	}
}
