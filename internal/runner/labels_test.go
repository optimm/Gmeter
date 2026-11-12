package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// labelled returns requests to the given paths, each labelled with its path.
func labelled(paths ...string) []Request {
	reqs := make([]Request, len(paths))
	for i, p := range paths {
		reqs[i] = Request{Target: p, Label: p}
	}
	return reqs
}

func labelsOf(res *Result) []string {
	var out []string
	for _, l := range res.Labels {
		out = append(out, fmt.Sprintf("%s=%d", l.Label, l.Scheduled))
	}
	return out
}

func TestResultBreaksDownByLabelBusiestFirst(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
		want  string
	}{
		{"busiest first, not alphabetical", []string{"/z", "/z", "/z", "/a"}, "/z=75 /a=25"},
		{"a tie goes in label order", []string{"/b", "/a"}, "/a=50 /b=50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := recordingServer(t)
			cfg := baseConfig(srv.URL) // 200 rps for 0.5s: 100 requests, cycling through the trace
			cfg.Requests = labelled(tc.paths...)

			res, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(labelsOf(res), " "); got != tc.want {
				t.Fatalf("labels = %q, want %q", got, tc.want)
			}
			var scheduled, samples uint64
			for _, l := range res.Labels {
				scheduled += l.Scheduled
				samples += l.Corrected.Count()
				if l.Corrected.Count() != l.Service.Count() || l.Corrected.Count() == 0 {
					t.Errorf("%s has %d corrected and %d service samples, want the same non-zero number",
						l.Label, l.Corrected.Count(), l.Service.Count())
				}
			}
			if scheduled != res.Scheduled || samples != res.Corrected.Count() {
				t.Errorf("labels add up to %d scheduled and %d samples, the run has %d and %d",
					scheduled, samples, res.Scheduled, res.Corrected.Count())
			}
		})
	}
}

func TestRunWithoutLabelsHasNoBreakdown(t *testing.T) {
	srv, _ := recordingServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Requests = []Request{{Target: "/a"}, {Target: "/b"}}

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Labels) != 0 {
		t.Errorf("Labels = %v, want none when no request has a label", labelsOf(res))
	}
}

func TestLabelResultsCountFailuresAndBadStatusesOfTheirOwnRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			w.WriteHeader(http.StatusInternalServerError)
		case "/drop": // close the connection without answering
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}
	}))
	t.Cleanup(srv.Close)
	cfg := baseConfig(srv.URL) // 100 requests over 3 labels: 34, 33 and 33
	cfg.Requests = labelled("/ok", "/bad", "/drop")

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]LabelResult{}
	for _, l := range res.Labels {
		by[l.Label] = l
	}
	ok, bad, drop := by["/ok"], by["/bad"], by["/drop"]
	if ok.Failed != 0 || ok.BadStatus != 0 {
		t.Errorf("/ok: failed %d, bad status %d, want 0 and 0", ok.Failed, ok.BadStatus)
	}
	if bad.BadStatus != bad.Scheduled || bad.Failed != 0 || bad.Scheduled == 0 {
		t.Errorf("/bad: scheduled %d, bad status %d, failed %d, want every request a bad status and none failed",
			bad.Scheduled, bad.BadStatus, bad.Failed)
	}
	if drop.Failed != drop.Scheduled || drop.BadStatus != 0 || drop.Scheduled == 0 {
		t.Errorf("/drop: scheduled %d, failed %d, bad status %d, want every request failed and none a bad status",
			drop.Scheduled, drop.Failed, drop.BadStatus)
	}
	if res.Statuses[500] != bad.Scheduled {
		t.Errorf("run saw %d 500s, /bad was sent %d times", res.Statuses[500], bad.Scheduled)
	}
}

// One slow endpoint makes the requests queued behind it late. Only the
// corrected latency of the others shows it: from the moment each was actually
// sent they were fast. This is the reason to read a breakdown with both.
func TestASlowLabelShowsUpInTheCorrectedLatencyOfTheOnesQueuedBehindIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(100 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := offsetsConfig(srv.URL, 0, 20*time.Millisecond, 40*time.Millisecond, 60*time.Millisecond)
	cfg.Workers = 1
	cfg.Requests = labelled("/slow", "/fast", "/slow", "/fast")

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]LabelResult{}
	for _, l := range res.Labels {
		by[l.Label] = l
	}
	fast := by["/fast"]
	if fast.Corrected.Count() != 2 {
		t.Fatalf("/fast has %d samples, want 2", fast.Corrected.Count())
	}
	if got := fast.Corrected.Min(); got < 60*time.Millisecond {
		t.Errorf("/fast corrected min = %v, want at least 60ms: each waited for a 100ms /slow request", got)
	}
	if got := fast.Service.Max(); got > 50*time.Millisecond {
		t.Errorf("/fast service max = %v, want under 50ms: once sent, it was answered at once", got)
	}
	if got := by["/slow"].Service.Min(); got < 90*time.Millisecond {
		t.Errorf("/slow service min = %v, want about 100ms", got)
	}
}

func TestLabelsMustBeOnEveryRequestOrNone(t *testing.T) {
	cfg := baseConfig("http://127.0.0.1:1")
	cfg.Requests = []Request{{Target: "/a", Label: "a"}, {Target: "/b"}}

	_, err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "request 2") || !strings.Contains(err.Error(), "Label") {
		t.Fatalf("err = %v, want one naming request 2 and Label", err)
	}
}

func TestThereIsALimitToHowManyLabelsAreTracked(t *testing.T) {
	cfg := baseConfig("http://127.0.0.1:1")
	for _, n := range []int{MaxLabels + 1, MaxLabels} {
		reqs := make([]Request, n)
		for i := range reqs {
			reqs[i] = Request{Target: "/", Label: fmt.Sprint("label", i)}
		}
		cfg.Requests = reqs
		_, err := cfg.normalize() // only the validation is under test: nothing is sent
		if tooMany := n > MaxLabels; tooMany != (err != nil) {
			t.Errorf("%d distinct labels: err = %v, want an error only above %d", n, err, MaxLabels)
		}
		if err != nil && !strings.Contains(err.Error(), fmt.Sprint(MaxLabels)) {
			t.Errorf("err = %v, want it to name the limit %d", err, MaxLabels)
		}
	}
}
