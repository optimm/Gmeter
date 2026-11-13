package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// collect is a Sink that keeps what it is given, as a caller's sink must be safe
// for concurrent use.
type collect struct {
	mu      sync.Mutex
	samples []Sample
}

func (c *collect) sink(s Sample) {
	c.mu.Lock()
	c.samples = append(c.samples, s)
	c.mu.Unlock()
}

func TestSinkGetsOneSampleForEveryRequestWithItsTimes(t *testing.T) {
	srv := okServer(t)
	var got collect
	cfg := baseConfig(srv.URL)
	cfg.Sink = got.sink

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	if uint64(len(got.samples)) != res.Scheduled || res.Scheduled != 100 {
		t.Fatalf("%d samples for %d scheduled requests, want 100 of each", len(got.samples), res.Scheduled)
	}
	seen := map[uint64]bool{}
	for _, s := range got.samples {
		if seen[s.Seq] {
			t.Fatalf("request %d was sampled twice", s.Seq)
		}
		seen[s.Seq] = true
		wantDue := time.Duration(s.Seq) * 5 * time.Millisecond // 200 requests a second, evenly
		switch {
		case s.Due != wantDue:
			t.Errorf("request %d is due at %v, want %v", s.Seq, s.Due, wantDue)
		case s.Sent < s.Due || s.Finished < s.Sent:
			t.Errorf("request %d: due %v, sent %v, finished %v: want them in that order", s.Seq, s.Due, s.Sent, s.Finished)
		case s.Status != http.StatusOK || s.Failure != "":
			t.Errorf("request %d: status %d, failure %q, want 200 and none", s.Seq, s.Status, s.Failure)
		}
	}
}

func TestSinkSeesAFailureWithItsKind(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close() // nothing listens there any more
	var got collect
	cfg := baseConfig(url)
	cfg.Duration = 100 * time.Millisecond
	cfg.Sink = got.sink

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	if len(got.samples) != 20 {
		t.Fatalf("%d samples, want 20", len(got.samples))
	}
	for _, s := range got.samples {
		if s.Failure != "connection_refused" || s.Status != 0 {
			t.Errorf("request %d: status %d, failure %q, want 0 and connection_refused", s.Seq, s.Status, s.Failure)
		}
	}
}

func TestSinkGetsTheCodeOfACallAndTheLabelOfARequest(t *testing.T) {
	var calls collect
	cfg := Config{URL: "svc", Rate: 100, Duration: 100 * time.Millisecond, Workers: 2, Timeout: time.Second, Sink: calls.sink,
		Call: func(context.Context) (string, error) { return "NotFound", nil }}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(calls.samples) != 10 || calls.samples[0].Code != "NotFound" || calls.samples[0].Status != 0 {
		t.Errorf("a call's samples: %+v, want 10 with code NotFound", calls.samples)
	}

	srv := okServer(t)
	var labelled collect
	cfg = baseConfig(srv.URL)
	cfg.Duration = 100 * time.Millisecond
	cfg.Requests = []Request{{Target: "/a", Label: "GET /a"}, {Target: "/b", Label: "GET /b"}}
	cfg.Sink = labelled.sink
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, s := range labelled.samples {
		if want := cfg.Requests[s.Seq%2].Label; s.Label != want {
			t.Errorf("request %d has label %q, want %q", s.Seq, s.Label, want)
		}
	}
}
