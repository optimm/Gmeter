package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const endpointTrace = `{"at":0,"path":"/orders/1?token=SECRET"}
{"at":0.02,"path":"/orders/2"}
{"at":0.04,"path":"/orders/3"}
{"at":0.06,"path":"/search?q=shoes"}
{"at":0.08,"path":"/search?q=boots"}
{"at":0.10,"path":"/health"}
`

// savedEndpoints runs a replay of url with the given flags and -json, and
// returns what it printed, the saved file's text and its endpoints' scheduled
// counts.
func savedEndpoints(t *testing.T, url string, flags ...string) (stdout, saved string, scheduled map[string]uint64) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "run.json")
	code, stdout, stderr := execReplay(t, append(flags, "-quiet", "-json", out, url)...)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Endpoints []struct {
			Endpoint  string `json:"endpoint"`
			Scheduled uint64 `json:"scheduled"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	scheduled = map[string]uint64{}
	for _, e := range got.Endpoints {
		scheduled[e.Endpoint] = e.Scheduled
	}
	return stdout, string(raw), scheduled
}

func TestReplayReportsEachEndpointAndKeepsQueryStringsOutOfThem(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", endpointTrace)

	stdout, saved, scheduled := savedEndpoints(t, srv.URL, "-file", trace)

	for _, want := range []string{"endpoints", "GET /orders/:id", "GET /search", "GET /health"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	if want := map[string]uint64{"GET /orders/:id": 3, "GET /search": 2, "GET /health": 1}; len(scheduled) != 3 ||
		scheduled["GET /orders/:id"] != 3 || scheduled["GET /search"] != 2 || scheduled["GET /health"] != 1 {
		t.Errorf("saved endpoints = %v, want %v", scheduled, want)
	}
	for _, secret := range []string{"SECRET", "shoes", "boots", "token"} {
		if strings.Contains(stdout, secret) || strings.Contains(saved, secret) {
			t.Errorf("%q is in the query string of the trace and must not reach the report or the saved file", secret)
		}
	}
}

func TestReplayEndpointsZeroLeavesTheBreakdownOut(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", endpointTrace)

	stdout, saved, _ := savedEndpoints(t, srv.URL, "-file", trace, "-endpoints", "0")

	if strings.Contains(stdout, "endpoint") || strings.Contains(saved, "endpoint") || strings.Contains(saved, "/orders") {
		t.Errorf("-endpoints 0 should keep every path out of the report and the saved file:\n%s\n%s", stdout, saved)
	}
}

func TestReplayEndpointsCapsTheRowsAndGroupsTheRest(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", endpointTrace)

	_, _, scheduled := savedEndpoints(t, srv.URL, "-file", trace, "-endpoints", "2")

	if len(scheduled) != 2 || scheduled["GET /orders/:id"] != 3 || scheduled["(other)"] != 3 {
		t.Errorf("endpoints = %v, want the busiest (3) and (other) (the other 3 requests)", scheduled)
	}
}

func TestReplayRanksEndpointsAfterLeavingOutTheMethodsItWillNotSend(t *testing.T) {
	srv, got := traceServer(t)
	var b strings.Builder
	for i, path := range []string{"/a", "/a", "/a", "/b", "/b"} {
		fmt.Fprintf(&b, `{"at":%g,"path":%q}`+"\n", 0.01*float64(i), path)
	}
	for i := range 10 { // the busiest endpoint by far, but writes: not replayed
		fmt.Fprintf(&b, `{"at":%g,"method":"POST","path":"/c"}`+"\n", 0.05+0.001*float64(i))
	}
	trace := writeTrace(t, "trace.jsonl", b.String())

	_, _, scheduled := savedEndpoints(t, srv.URL, "-file", trace, "-endpoints", "2")

	if len(scheduled) != 2 || scheduled["GET /a"] != 3 || scheduled["GET /b"] != 2 {
		t.Errorf("endpoints = %v, want GET /a (3) and GET /b (2) and nothing for the POSTs that were not sent", scheduled)
	}
	for _, s := range got() {
		if s.method == "POST" {
			t.Errorf("a POST was sent: %v", s)
		}
	}
}

func TestReplayAtAFixedRateReportsEndpointsInProportionToTheCycledTrace(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "untimed.jsonl", `{"path":"/a"}`+"\n"+`{"path":"/a"}`+"\n"+`{"path":"/a"}`+"\n"+`{"path":"/b"}`+"\n")

	_, _, scheduled := savedEndpoints(t, srv.URL, "-file", trace, "-rate", "100", "-duration", "400ms", "-arrivals", "even")

	if scheduled["GET /a"] != 30 || scheduled["GET /b"] != 10 {
		t.Errorf("endpoints = %v, want 30 and 10: 40 requests cycling through a trace that is 3/4 /a", scheduled)
	}
}

func TestReplayReportsAtMostTwentyEndpointRowsByDefault(t *testing.T) {
	srv, _ := traceServer(t)
	var b strings.Builder
	for i := range 25 { // 25 different endpoints, one request each
		fmt.Fprintf(&b, `{"at":%g,"path":"/page-%c"}`+"\n", 0.004*float64(i), 'a'+i)
	}
	trace := writeTrace(t, "many.jsonl", b.String())

	_, _, scheduled := savedEndpoints(t, srv.URL, "-file", trace)

	if len(scheduled) != 20 || scheduled["(other)"] != 6 {
		t.Errorf("%d rows, (other) = %d, want 20 rows with the 6 requests that did not fit (the 19 busiest get their own)", len(scheduled), scheduled["(other)"])
	}
}
