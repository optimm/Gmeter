package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/runner"
)

// labelResult is one endpoint's results: n samples, all at the given latencies.
func labelResult(name string, scheduled, failed, bad uint64, n int, corrected, service time.Duration) runner.LabelResult {
	c, s := hist.New(), hist.New()
	for range n {
		c.Record(corrected)
		s.Record(service)
	}
	return runner.LabelResult{Label: name, Scheduled: scheduled, Failed: failed, BadStatus: bad, Corrected: c, Service: s}
}

// endpointResult is a run of three endpoints, busiest first.
func endpointResult() *runner.Result {
	res := sampleResult()
	res.Labels = []runner.LabelResult{
		labelResult("GET /orders/:id", 80, 0, 2, 80, 5*time.Millisecond, 5*time.Millisecond),
		labelResult("GET /search", 15, 2, 0, 13, 200*time.Millisecond, 20*time.Millisecond),
		labelResult("(other)", 10, 0, 0, 10, time.Millisecond, time.Millisecond),
	}
	return res
}

func TestSummarizeBreaksTheRunDownByEndpoint(t *testing.T) {
	s := Summarize(endpointResult(), 10)

	if len(s.Endpoints) != 3 {
		t.Fatalf("%d endpoints, want 3", len(s.Endpoints))
	}
	orders, search := s.Endpoints[0], s.Endpoints[1]
	if orders.Endpoint != "GET /orders/:id" || s.Endpoints[1].Endpoint != "GET /search" || s.Endpoints[2].Endpoint != "(other)" {
		t.Errorf("endpoints = %q, %q, %q, want them in the order given, busiest first",
			orders.Endpoint, search.Endpoint, s.Endpoints[2].Endpoint)
	}
	if orders.Scheduled != 80 || orders.BadStatus != 2 || orders.Failed != 0 {
		t.Errorf("/orders scheduled/bad/failed = %d/%d/%d, want 80/2/0", orders.Scheduled, orders.BadStatus, orders.Failed)
	}
	if search.Scheduled != 15 || search.BadStatus != 0 || search.Failed != 2 {
		t.Errorf("/search scheduled/bad/failed = %d/%d/%d, want 15/0/2", search.Scheduled, search.BadStatus, search.Failed)
	}
	if search.Corrected.Count != 13 || search.Corrected.P50Ms != 200 || search.Service.P99Ms != 20 {
		t.Errorf("/search corrected count %d p50 %vms, service p99 %vms, want 13, 200 and 20",
			search.Corrected.Count, search.Corrected.P50Ms, search.Service.P99Ms)
	}
}

// endpointRows returns the numbers of each table row (scheduled, bad, failed, p50, p99, max, svc p99)
// by endpoint name, and the header line.
func endpointRows(t *testing.T, out string) (rows map[string][]string, header string) {
	t.Helper()
	_, section, found := strings.Cut(out, "endpoints")
	if !found {
		t.Fatalf("no endpoint table in:\n%s", out)
	}
	rows = map[string][]string{}
	lines := strings.Split(section, "\n")
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		switch {
		case line == "":
			return rows, header
		case header == "":
			header = line
		case len(f) >= 8:
			rows[strings.Join(f[:len(f)-7], " ")] = f[len(f)-7:]
		}
	}
	return rows, header
}

func TestTextReportShowsAnEndpointTableForSeveralEndpoints(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(endpointResult(), 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	rows, header := endpointRows(t, buf.String())

	for _, col := range []string{"endpoint", "scheduled", "bad", "failed", "p50", "p99", "max", "svc p99"} {
		if !strings.Contains(header, col) {
			t.Errorf("header %q has no %q column", header, col)
		}
	}
	for name, want := range map[string][]string{
		"GET /orders/:id": {"80", "2", "0", "5.0ms", "5.0ms", "5.0ms", "5.0ms"},
		"GET /search":     {"15", "0", "2", "200.0ms", "200.0ms", "200.0ms", "20.0ms"},
		"(other)":         {"10", "0", "0", "1.0ms", "1.0ms", "1.0ms", "1.0ms"},
	} {
		if got := rows[name]; strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s row = %v, want %v", name, got, want)
		}
	}
	if !strings.Contains(buf.String(), "busiest first") || !strings.Contains(buf.String(), "corrected") {
		t.Errorf("the heading should say how to read the table:\n%s", buf.String())
	}
}

func TestTextReportHasNoEndpointTableUnlessThereAreSeveral(t *testing.T) {
	one := sampleResult()
	one.Labels = endpointResult().Labels[:1] // a table of one row would only repeat the run's numbers
	for name, res := range map[string]*runner.Result{"no endpoints": sampleResult(), "one endpoint": one} {
		var buf bytes.Buffer
		if err := Summarize(res, 10).WriteText(&buf); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "endpoint") {
			t.Errorf("%s: the report should not have an endpoint table:\n%s", name, buf.String())
		}
	}
}

func TestEndpointTableShortensLongNamesWithoutLosingTheNumbers(t *testing.T) {
	long := "GET /" + strings.Repeat("segment/", 12)
	res := sampleResult()
	res.Labels = []runner.LabelResult{
		labelResult(long, 5, 0, 0, 5, 7*time.Millisecond, 7*time.Millisecond),
		labelResult("GET /a", 4, 0, 0, 4, 7*time.Millisecond, 7*time.Millisecond),
	}
	var buf bytes.Buffer
	if err := Summarize(res, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(out, long[:40]) {
		t.Errorf("the long name was printed in full:\n%s", out)
	}
	if !strings.Contains(out, long[:35]+"…") {
		t.Errorf("the long name should be cut to 35 characters and an ellipsis:\n%s", out)
	}
	_, header := endpointRows(t, out)
	_, section, _ := strings.Cut(out, "endpoints")
	for _, line := range strings.Split(section, "\n")[2:4] { // the two rows
		if utf8.RuneCountInString(line) != utf8.RuneCountInString(header) {
			t.Errorf("row %q is not as wide as the header %q: the columns are out of line", line, header)
		}
	}

	if w := utf8.RuneCountInString(header); w > 110 {
		t.Errorf("the table is %d columns wide, want it to fit a 110-column terminal whatever the names", w)
	}

	if got := Summarize(res, 10).Endpoints[0].Endpoint; got != long {
		t.Errorf("the saved name was shortened to %q: only the text report may do that", got)
	}
}

func TestEndpointWithoutResponsesShowsDashesNotZeros(t *testing.T) {
	res := sampleResult()
	res.Labels = []runner.LabelResult{
		labelResult("GET /a", 5, 0, 0, 5, 7*time.Millisecond, 7*time.Millisecond),
		labelResult("GET /dead", 5, 5, 0, 0, 0, 0), // every request failed to connect
	}
	var buf bytes.Buffer
	if err := Summarize(res, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	rows, _ := endpointRows(t, buf.String())
	if got := strings.Join(rows["GET /dead"], " "); got != "5 0 5 - - - -" {
		t.Errorf("GET /dead row = %q, want latencies shown as - when there were no samples", got)
	}
}

func TestJSONHasEndpointsOnlyForRunsThatHadThem(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(endpointResult(), 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Endpoints []struct {
			Endpoint  string `json:"endpoint"`
			Scheduled uint64 `json:"scheduled"`
			Failed    uint64 `json:"failed"`
			BadStatus uint64 `json:"bad_status"`
			Corrected struct {
				P99Ms float64 `json:"p99_ms"`
			} `json:"latency_corrected"`
			Service struct {
				P99Ms float64 `json:"p99_ms"`
			} `json:"latency_service"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Endpoints) != 3 {
		t.Fatalf("JSON has %d endpoints, want 3:\n%s", len(raw.Endpoints), buf.String())
	}
	if e := raw.Endpoints[1]; e.Endpoint != "GET /search" || e.Scheduled != 15 || e.Failed != 2 || e.Corrected.P99Ms != 200 || e.Service.P99Ms != 20 {
		t.Errorf("endpoints[1] = %+v, want GET /search with 15 scheduled, 2 failed, p99 200ms corrected and 20ms service", e)
	}
	back, err := ReadSummary(&buf)
	if err != nil || len(back.Endpoints) != 3 {
		t.Errorf("ReadSummary: %d endpoints, err %v, want the 3 that were written", len(back.Endpoints), err)
	}

	buf.Reset()
	if err := Summarize(sampleResult(), 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"endpoints"`) {
		t.Errorf("a run without endpoints should not have the key at all:\n%s", buf.String())
	}
}
