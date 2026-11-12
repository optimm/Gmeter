package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A replayed id that does not exist in the target answers 404 where the log's
// server answered 200. The report has to show how much of the replay that is.
func TestReplayShowsHowManyResponsesDifferFromTheLoggedStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orders/1002" || r.URL.Path == "/orders/1003" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	trace := writeTrace(t, "trace.jsonl", `{"at":0,"path":"/orders/1001","status":200}
{"at":0.01,"path":"/orders/1002","status":200}
{"at":0.02,"path":"/orders/1003","status":201}
{"at":0.03,"path":"/search?q=x","status":200}
{"at":0.04,"path":"/search?q=y"}
`)
	out := filepath.Join(t.TempDir(), "run.json")

	code, stdout, stderr := execReplay(t, "-file", trace, "-quiet", "-json", out, srv.URL)

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	// 4 of the 5 requests have a logged status; two of those got a 404 for a logged 2xx.
	for _, want := range []string{
		"vs log     4 compared, 2 differ (50.0%): 2xx→4xx 2",
		"were of a different class than the log recorded",
		"endpoints, status vs the log",
		"2 of 3",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		VsLog struct {
			Compared, Different uint64
			Changes             []struct {
				Logged, Got string
				Count       uint64
			}
		} `json:"vs_log"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.VsLog.Compared != 4 || saved.VsLog.Different != 2 || len(saved.VsLog.Changes) != 1 || saved.VsLog.Changes[0].Got != "4xx" {
		t.Errorf("saved vs_log = %+v, want 4 compared, 2 different, one change to 4xx", saved.VsLog)
	}
}

func TestReplayOfAnAccessLogComparesItsStatusesToo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	trace := writeTrace(t, "access.log", `127.0.0.1 - - [10/Oct/2023:13:55:36 +0000] "GET /a HTTP/1.1" 200 5
127.0.0.1 - - [10/Oct/2023:13:55:36 +0000] "GET /b HTTP/1.1" 500 5
127.0.0.1 - - [10/Oct/2023:13:55:37 +0000] "GET /c HTTP/1.1" 200 5
`)

	code, stdout, stderr := execReplay(t, "-file", trace, "-quiet", "-arrivals", "even", srv.URL)

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "vs log     3 compared, 2 differ (66.7%): 2xx→5xx 2") {
		t.Errorf("want the two 200s that now answer 500 counted, and the 500 that still does not:\n%s", stdout)
	}
}

func TestReplayWithoutLoggedStatusesSaysNothingOfTheLog(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)

	_, stdout, _ := execReplay(t, "-file", trace, "-methods", "GET,POST", "-quiet", srv.URL)

	if strings.Contains(stdout, "vs log") {
		t.Errorf("a line about the log although the trace has no statuses:\n%s", stdout)
	}
}
