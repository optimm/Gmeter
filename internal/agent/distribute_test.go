package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// countingTarget counts the requests it gets, by path.
func countingTarget(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestAgentsTogetherSendWhatOneGeneratorWould(t *testing.T) {
	srv, got := countingTarget(t)
	agents := []*Client{start(t, newServer(t)), start(t, newServer(t)), start(t, newServer(t))}
	res, err := Distribute(context.Background(), agents, job(srv.URL)) // 100 rps for 300ms
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 30 || got.Load() != 30 || res.Statuses[200] != 30 || res.Corrected.Count() != 30 {
		t.Errorf("scheduled %d, the server got %d, statuses %v; want the 30 of 100 rps for 300ms", res.Scheduled, got.Load(), res.Statuses)
	}
	if st := res.Stages[0].Stage; st.From != 100 || st.Duration != 300*time.Millisecond {
		t.Errorf("stage %v, want the whole test's 300ms@100", st)
	}
}

func TestAgentsTogetherReplayATrace(t *testing.T) {
	srv, got := countingTarget(t)
	j := job(srv.URL)
	j.Stages = nil
	for i := range 31 { // a share of 31 does not end where the others do
		j.Offsets = append(j.Offsets, time.Duration(i)*10*time.Millisecond)
		label := "GET /even"
		if i%2 == 1 {
			label = "GET /odd"
		}
		j.Requests = append(j.Requests, runner.Request{Target: "/x", Label: label})
	}
	agents := []*Client{start(t, newServer(t)), start(t, newServer(t))}
	res, err := Distribute(context.Background(), agents, j)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 31 || got.Load() != 31 {
		t.Errorf("scheduled %d, the server got %d; want the trace's 31", res.Scheduled, got.Load())
	}
	if len(res.Labels) != 2 || res.Labels[0].Scheduled != 16 || res.Labels[1].Scheduled != 15 {
		t.Errorf("labels %+v, want 16 GET /even and 15 GET /odd", res.Labels)
	}
	if st := res.Stages[0].Stage; st.Duration != 300*time.Millisecond {
		t.Errorf("stage %v, want the whole trace's 300ms", st)
	}
}

func TestADistributedTestStartsOnlyWhenEveryAgentCanRunIt(t *testing.T) {
	var calls atomic.Int32
	good := newServer(t)
	good.run = fakeRun(&calls)
	busy := newServer(t)
	busy.busy.Store(true)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":2,"busy":false}`))
	}))
	defer other.Close()
	url := target(t).URL

	for name, tc := range map[string]struct {
		second Client
		job    Job
		want   string
	}{
		"busy":            {*start(t, busy), job(url), "busy"},
		"unreachable":     {*NewClient(gone.URL, token), job(url), gone.Listener.Addr().String()},
		"wrong password":  {*NewClient(start(t, newServer(t)).base, "not-the-password-at-all"), job(url), "password"},
		"another version": {*NewClient(other.URL, token), job(url), "version"},
		"a bad job":       {*start(t, newServer(t)), func() Job { j := job(url); j.URL = "ftp://x/"; return j }(), "url"},
		// Checked here before asking the agents: their being away is not the problem.
		"a bad job, and an agent away": {*NewClient(gone.URL, token), func() Job { j := job(url); j.URL = "ftp://x/"; return j }(), "url"},
	} {
		second := tc.second
		_, err := Distribute(context.Background(), []*Client{start(t, good), &second}, tc.job)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error that says %q", name, err, tc.want)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("an agent ran %d jobs of tests that could not run everywhere", calls.Load())
	}
}

// blockingRun runs until it is cancelled, and says so on stopped.
func blockingRun(stopped chan<- struct{}) func(context.Context, runner.Config) (*runner.Result, error) {
	return func(ctx context.Context, _ runner.Config) (*runner.Result, error) {
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
}

func TestOneAgentFailingStopsTheOthers(t *testing.T) {
	failing := newServer(t)
	failing.run = func(context.Context, runner.Config) (*runner.Result, error) {
		return nil, errors.New("out of file descriptors")
	}
	stopped := make(chan struct{})
	waiting := newServer(t)
	waiting.run = blockingRun(stopped)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Distribute(ctx, []*Client{start(t, waiting), start(t, failing)}, job(target(t).URL))
	if err == nil || !strings.Contains(err.Error(), "out of file descriptors") {
		t.Errorf("Distribute said %v, want the failing agent's error", err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("the other agent kept running")
	}
}

func TestStoppingADistributedTestStopsEveryAgent(t *testing.T) {
	stopped := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var agents []*Client
	for _, ch := range stopped {
		s := newServer(t)
		s.run = blockingRun(ch)
		agents = append(agents, start(t, s))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := Distribute(ctx, agents, job(target(t).URL))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Distribute said %v, want the context's error", err)
	}
	for i, ch := range stopped {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Errorf("agent %d kept running", i+1)
		}
	}
}

func TestADistributedTestNamesTheAgentThatDoesNotAnswer(t *testing.T) {
	defer func(d time.Duration) { pingTimeout = d }(pingTimeout)
	pingTimeout = 200 * time.Millisecond
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer silent.Close()

	_, err := Distribute(context.Background(), []*Client{start(t, newServer(t)), NewClient(silent.URL, token)}, job(target(t).URL))
	if err == nil || !strings.Contains(err.Error(), silent.Listener.Addr().String()) || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("Distribute said %v, want which agent did not answer", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Distribute(ctx, []*Client{NewClient(silent.URL, token)}, job(target(t).URL)); err != context.DeadlineExceeded {
		t.Errorf("stopped while asking the agents: %v, want the context's error", err)
	}
}
