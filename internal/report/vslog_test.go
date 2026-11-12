package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// loggedResult is a replay whose responses were compared with the log: of 200,
// 30 were 4xx and 10 were 5xx where the log has 2xx.
func loggedResult() *runner.Result {
	res := sampleResult()
	res.Logged = runner.LoggedStatuses{Compared: 200, Changes: []runner.StatusChange{
		{Logged: 2, Got: 4, Count: 30}, {Logged: 2, Got: 5, Count: 10},
	}}
	return res
}

func TestSummarizeComparesTheResponsesWithTheLog(t *testing.T) {
	s := Summarize(loggedResult(), 10)

	if s.VsLog == nil || s.VsLog.Compared != 200 || s.VsLog.Different != 40 {
		t.Fatalf("VsLog = %+v, want 200 compared and 40 different", s.VsLog)
	}
	if got := s.VsLog.Changes; len(got) != 2 || got[0] != (LogChange{Logged: "2xx", Got: "4xx", Count: 30}) || got[1] != (LogChange{Logged: "2xx", Got: "5xx", Count: 10}) {
		t.Errorf("Changes = %v, want 2xx to 4xx 30 then 2xx to 5xx 10", got)
	}
}

func TestSummaryHasNoVsLogWhenNothingWasCompared(t *testing.T) {
	s := Summarize(sampleResult(), 10)
	if s.VsLog != nil {
		t.Errorf("VsLog = %+v, want none", s.VsLog)
	}
	var buf bytes.Buffer
	if err := s.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "vs_log") {
		t.Errorf("the JSON has a vs_log key although nothing was compared:\n%s", buf.String())
	}
}

func TestVsLogSurvivesTheJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(loggedResult(), 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Summary
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.VsLog == nil || back.VsLog.Different != 40 || len(back.VsLog.Changes) != 2 || back.VsLog.Changes[0].Got != "4xx" {
		t.Errorf("after a round trip VsLog = %+v", back.VsLog)
	}
}

func TestReportWarnsWhenAFifthOfTheResponsesDifferFromTheLog(t *testing.T) {
	for _, tc := range []struct {
		differ uint64
		warn   bool
	}{{4, false}, {5, true}, {40, true}} { // of 100 compared
		res := sampleResult()
		res.Logged = runner.LoggedStatuses{Compared: 100, Changes: []runner.StatusChange{{Logged: 2, Got: 4, Count: tc.differ}}}

		s := Summarize(res, 10)

		if tc.warn != (len(s.Warnings) == 1) {
			t.Errorf("%d of 100 differ: warnings = %v, want a warning: %v", tc.differ, s.Warnings, tc.warn)
		}
		if tc.warn && len(s.Warnings) == 1 {
			for _, want := range []string{"of 100 responses", "different class than the log", "2xx→4xx"} {
				if !strings.Contains(s.Warnings[0], want) {
					t.Errorf("the warning %q does not mention %q", s.Warnings[0], want)
				}
			}
		}
	}
}

func TestTextReportShowsHowManyResponsesDifferFromTheLog(t *testing.T) {
	var buf strings.Builder
	if err := Summarize(loggedResult(), 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if want := "vs log     200 compared, 40 differ (20.0%): 2xx→4xx 30, 2xx→5xx 10\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("the report has no line %q:\n%s", want, buf.String())
	}

	buf.Reset()
	if err := Summarize(sampleResult(), 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "vs log") {
		t.Errorf("a line about the log although nothing was compared:\n%s", buf.String())
	}
}

func TestTextReportShortensALongListOfChanges(t *testing.T) {
	res := sampleResult()
	res.Logged = runner.LoggedStatuses{Compared: 1000, Changes: []runner.StatusChange{
		{Logged: 2, Got: 4, Count: 6}, {Logged: 2, Got: 5, Count: 5}, {Logged: 3, Got: 4, Count: 4},
		{Logged: 2, Got: 3, Count: 3}, {Logged: 4, Got: 2, Count: 2}, {Logged: 5, Got: 2, Count: 1},
	}}
	var buf strings.Builder
	if err := Summarize(res, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if want := "21 differ (2.1%): 2xx→4xx 6, 2xx→5xx 5, 3xx→4xx 4, 2xx→3xx 3, and 2 more"; !strings.Contains(buf.String(), want) {
		t.Errorf("want the four most common and a count of the rest (%q):\n%s", want, buf.String())
	}
}

// loggedEndpoints is three endpoints of which two differ from the log: /gone in
// 90 of 100 (the larger share), /slow in 120 of 400 (the larger number); /fine in
// none.
func loggedEndpoints() *runner.Result {
	res := loggedResult()
	a := labelResult("GET /slow", 400, 0, 0, 400, time.Millisecond, time.Millisecond)
	a.LoggedCompared, a.LoggedDiffer = 400, 120
	b := labelResult("GET /gone", 100, 0, 0, 100, time.Millisecond, time.Millisecond)
	b.LoggedCompared, b.LoggedDiffer = 100, 90
	c := labelResult("GET /fine", 50, 0, 0, 50, time.Millisecond, time.Millisecond)
	c.LoggedCompared = 50
	res.Labels = []runner.LabelResult{a, b, c}
	return res
}

func TestEndpointsCarryTheirComparisonWithTheLog(t *testing.T) {
	s := Summarize(loggedEndpoints(), 10)

	if e := s.Endpoints[1]; e.Endpoint != "GET /gone" || e.LogCompared != 100 || e.LogDifferent != 90 {
		t.Errorf("GET /gone = %+v, want 100 compared and 90 different", e)
	}
	var buf bytes.Buffer
	if err := Summarize(endpointResult(), 10).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "log_compared") {
		t.Errorf("an endpoint has log_compared although nothing was compared:\n%s", buf.String())
	}
}

func TestEndpointsThatDifferFromTheLogAreListedWorstFirst(t *testing.T) {
	var buf strings.Builder
	if err := Summarize(loggedEndpoints(), 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	_, section, found := strings.Cut(out, "endpoints, status vs the log")
	if !found {
		t.Fatalf("no section about endpoints and the log:\n%s", out)
	}
	section, _, _ = strings.Cut(section, "\n\n")
	gone, slow := strings.Index(section, "GET /gone"), strings.Index(section, "GET /slow")
	if gone < 0 || slow < 0 || gone > slow {
		t.Errorf("want /gone (90%% of its requests) listed before /slow (30%%, but more of them):\n%s", section)
	}
	if strings.Contains(section, "GET /fine") {
		t.Errorf("GET /fine matched the log and should not be listed:\n%s", section)
	}
	for _, want := range []string{"90 of 100", "90.0%", "120 of 400", "30.0%"} {
		if !strings.Contains(section, want) {
			t.Errorf("the section is missing %q:\n%s", want, section)
		}
	}
}

func TestNoEndpointSectionWhenNoneDifferFromTheLog(t *testing.T) {
	res := loggedEndpoints()
	for i := range res.Labels {
		res.Labels[i].LoggedDiffer = 0
	}
	var buf strings.Builder
	if err := Summarize(res, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "endpoints, status vs the log") {
		t.Errorf("a section although no endpoint differs:\n%s", buf.String())
	}
}

func TestTextLineHasNoListOfChangesWhenNoneDiffer(t *testing.T) {
	res := sampleResult()
	res.Logged = runner.LoggedStatuses{Compared: 100}
	var buf strings.Builder
	if err := Summarize(res, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if want := "vs log     100 compared, 0 differ (0.0%)\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("want the line %q with nothing after it:\n%s", want, buf.String())
	}
}

// One endpoint is the whole run, so its section would only repeat the line above.
func TestNoEndpointSectionForASingleEndpoint(t *testing.T) {
	res := loggedEndpoints()
	res.Labels = res.Labels[1:2] // just /gone
	var buf strings.Builder
	if err := Summarize(res, 10).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "endpoints, status vs the log") {
		t.Errorf("a section for one endpoint:\n%s", buf.String())
	}
}
