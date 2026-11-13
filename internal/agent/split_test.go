package agent

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

// trace is n requests whose targets say their index.
func trace(n int) []runner.Request {
	out := make([]runner.Request, n)
	for i := range out {
		out[i] = runner.Request{Target: fmt.Sprintf("/%d", i)}
	}
	return out
}

func TestSplittingAProfileGivesEachAgentAShareOfTheRate(t *testing.T) {
	whole := Job{
		URL: "http://x/", Method: http.MethodPost, Header: http.Header{"X-A": {"1"}}, Body: []byte("b"),
		Stages:  []pacer.Stage{{Duration: time.Second, From: 30, To: 90}, {Duration: 2 * time.Second, From: 60, To: 60}},
		Workers: 10, Timeout: time.Second, Spin: time.Millisecond, Phases: true, HTTP2: true, Insecure: true,
		SampleEvery: time.Second, StartIn: time.Second,
	}
	jobs, err := whole.Split(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("%d jobs, want 3", len(jobs))
	}
	workers := 0
	for i, j := range jobs {
		want := []pacer.Stage{{Duration: time.Second, From: 10, To: 30}, {Duration: 2 * time.Second, From: 20, To: 20}}
		if !reflect.DeepEqual(j.Stages, want) {
			t.Errorf("agent %d: stages %v, want %v", i+1, j.Stages, want)
		}
		if wantPhase := float64(i) / 3; j.Phase != wantPhase {
			t.Errorf("agent %d: phase %v, want %v so that the agents take turns instead of firing together", i+1, j.Phase, wantPhase)
		}
		workers += j.Workers
		if j.Workers < 3 || j.Workers > 4 {
			t.Errorf("agent %d: %d workers, want 3 or 4 of the 10", i+1, j.Workers)
		}
		// Everything else is the whole test's.
		same := j
		same.Stages, same.Phase, same.Workers = whole.Stages, whole.Phase, whole.Workers
		if !reflect.DeepEqual(same, whole) {
			t.Errorf("agent %d: %+v, want the rest as in %+v", i+1, same, whole)
		}
	}
	if workers != 10 {
		t.Errorf("%d workers in all, want the 10 asked for", workers)
	}
	if whole.Stages[0].From != 30 {
		t.Error("Split changed the job it split")
	}
}

func TestSplittingRandomArrivalsGivesEachAgentItsOwnSeed(t *testing.T) {
	for _, arrivals := range []string{"poisson", " Poisson"} { // as the agent reads it
		whole := Job{URL: "http://x/", Stages: pacer.ConstantRate(100, time.Second), Arrivals: arrivals, Seed: 7, Workers: 4}
		jobs, err := whole.Split(2)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 2 || jobs[0].Seed != 7 || jobs[1].Seed != 8 || jobs[0].Phase != 0 || jobs[1].Phase != 0 {
			t.Errorf("%q: %+v; want seeds 7 and 8 (the same seed would send the same bursts twice), and no phases", arrivals, jobs)
		}
	}
	bad := Job{URL: "http://x/", Stages: pacer.ConstantRate(100, time.Second), Arrivals: "bursty", Workers: 4}
	if _, err := bad.Split(2); err == nil {
		t.Error("split a job of unknown arrivals")
	}
}

func TestSplittingARecordedTraceDealsItsRequestsOut(t *testing.T) {
	offsets := []time.Duration{0, 10, 20, 30, 40, 50, 60}
	whole := Job{URL: "http://x/", Offsets: offsets, Requests: trace(7), Workers: 3}
	jobs, err := whole.Split(3)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[time.Duration]string{}
	for i, j := range jobs {
		if j.Span != 60 {
			t.Errorf("agent %d: span %v, want the whole trace's, 60ns, so the results line up", i+1, j.Span)
		}
		for k, off := range j.Offsets {
			if k > 0 && off-j.Offsets[k-1] != 30 {
				t.Errorf("agent %d: offsets %v, want every third request", i+1, j.Offsets)
			}
			seen[off] = j.Requests[k%len(j.Requests)].Target
		}
	}
	for i, off := range offsets {
		if want := fmt.Sprintf("/%d", i); seen[off] != want {
			t.Errorf("the request due at %v is %q on the agents, %q in the trace", off, seen[off], want)
		}
	}
}

func TestSplittingACycledTraceKeepsItsOrderAcrossTheAgents(t *testing.T) {
	// At a fixed rate the trace is cycled: the k-th request of the whole test
	// sends request k mod len. With even arrivals agent i sends the whole
	// test's requests i, n+i, 2n+i, ...: they must be the same requests.
	// Each agent carries no more of the trace than it needs: a trace can be long.
	for _, tc := range []struct{ requests, agents, carried int }{{4, 2, 2}, {3, 2, 3}, {6, 4, 3}, {5, 5, 1}, {2, 3, 2}} {
		whole := Job{URL: "http://x/", Stages: pacer.ConstantRate(100, time.Second), Requests: trace(tc.requests), Workers: tc.agents}
		jobs, err := whole.Split(tc.agents)
		if err != nil {
			t.Fatal(err)
		}
		for i, j := range jobs {
			if len(j.Requests) != tc.carried {
				t.Errorf("%d requests on %d agents: agent %d carries %d, want %d", tc.requests, tc.agents, i+1, len(j.Requests), tc.carried)
			}
			for k := range 3 * tc.requests {
				got := j.Requests[k%len(j.Requests)].Target
				if want := whole.Requests[(tc.agents*k+i)%tc.requests].Target; got != want {
					t.Fatalf("%d requests on %d agents: agent %d's request %d is %s, want %s", tc.requests, tc.agents, i+1, k, got, want)
				}
			}
		}
	}
}

func TestSplittingRefusesWhatCannotBeShared(t *testing.T) {
	for name, tc := range map[string]struct {
		job Job
		n   int
	}{
		"no agents":                  {Job{Stages: pacer.ConstantRate(1, time.Second), Workers: 1}, 0},
		"fewer workers than agents":  {Job{Stages: pacer.ConstantRate(1, time.Second), Workers: 2}, 3},
		"fewer requests than agents": {Job{Offsets: []time.Duration{0, 1}, Requests: trace(2), Workers: 3}, 3},
	} {
		if _, err := tc.job.Split(tc.n); err == nil {
			t.Errorf("%s: split", name)
		}
	}
}
