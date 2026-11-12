package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
)

// stagePhaseTotal is the total of one phase in a stage's results.
func stagePhaseTotal(t *testing.T, st StageResult, name string) PhaseTotal {
	t.Helper()
	for _, p := range st.Phases {
		if p.Phase == name {
			return p
		}
	}
	t.Fatalf("no phase %q in %v", name, st.Phases)
	return PhaseTotal{}
}

func TestEachStageSplitsItsOwnRequestsIntoPhases(t *testing.T) {
	// The first 20 requests are answered at once, every later one after 20ms: the
	// second stage's wait must be its own, not the run's average.
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 20 {
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := Config{
		URL:     srv.URL,
		Workers: 8,
		Stages: []pacer.Stage{
			{Duration: 200 * time.Millisecond, From: 100, To: 100}, // 20 requests
			{Duration: 200 * time.Millisecond, From: 100, To: 100},
		},
		Phases: true,
	}

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stages) != 2 {
		t.Fatalf("%d stages, want 2", len(res.Stages))
	}
	for i, st := range res.Stages {
		if len(st.Phases) != len(PhaseNames) {
			t.Fatalf("stage %d has %d phases, want %d", i+1, len(st.Phases), len(PhaseNames))
		}
		var total time.Duration
		for j, p := range st.Phases {
			if p.Phase != PhaseNames[j] {
				t.Errorf("stage %d phase %d is %q, want %q", i+1, j, p.Phase, PhaseNames[j])
			}
			total += p.Total
		}
		if total != st.Corrected.Sum() {
			t.Errorf("stage %d: the phases add up to %v, its corrected latencies to %v", i+1, total, st.Corrected.Sum())
		}
		if q := stagePhaseTotal(t, st, "queue").Requests; q != st.Corrected.Count() {
			t.Errorf("stage %d: queue has %d requests, want one per response (%d)", i+1, q, st.Corrected.Count())
		}
	}
	mean := func(p PhaseTotal) time.Duration { return p.Total / time.Duration(max(p.Requests, 1)) }
	if w := mean(stagePhaseTotal(t, res.Stages[0], "wait")); w > 10*time.Millisecond {
		t.Errorf("stage 1 mean wait = %v, want the fast answers only", w)
	}
	if w := mean(stagePhaseTotal(t, res.Stages[1], "wait")); w < 19*time.Millisecond {
		t.Errorf("stage 2 mean wait = %v, want at least the server's 20ms", w)
	}
}

func TestStagesHaveNoPhasesUnlessAskedFor(t *testing.T) {
	srv, _ := recordingServer(t)
	res, err := Run(context.Background(), baseConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range res.Stages {
		if st.Phases != nil {
			t.Errorf("stage %d Phases = %v, want nil when Config.Phases is not set", i+1, st.Phases)
		}
	}
}
