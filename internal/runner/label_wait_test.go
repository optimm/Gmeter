package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestEachLabelKeepsTheDistributionOfItsWait(t *testing.T) {
	// One /spiky request in ten takes 80ms, the rest 1ms: the mean wait is about
	// 9ms, but the tail is the 80ms the mean hides.
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/spiky" && n.Add(1)%10 == 0 {
			time.Sleep(80 * time.Millisecond)
			return
		}
		time.Sleep(time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	cfg := baseConfig(srv.URL) // 100 requests, 50 to each label
	cfg.Requests = labelled("/spiky", "/steady")
	cfg.Phases = true

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	spiky, steady := labelNamed(t, res, "/spiky"), labelNamed(t, res, "/steady")
	for _, l := range []LabelResult{spiky, steady} {
		if l.Wait == nil {
			t.Fatalf("%s has no wait distribution", l.Label)
		}
		if l.Wait.Count() != l.Corrected.Count() {
			t.Errorf("%s: %d wait samples, want one per response (%d)", l.Label, l.Wait.Count(), l.Corrected.Count())
		}
		if l.Wait.Sum() != labelPhase(t, l, "wait").Total {
			t.Errorf("%s: the wait samples add up to %v, the wait phase to %v", l.Label, l.Wait.Sum(), labelPhase(t, l, "wait").Total)
		}
	}
	if p99 := spiky.Wait.Quantile(0.99); p99 < 75*time.Millisecond {
		t.Errorf("/spiky wait p99 = %v, want the 80ms tail", p99)
	}
	if p99 := steady.Wait.Quantile(0.99); p99 > 40*time.Millisecond {
		t.Errorf("/steady wait p99 = %v, want far below /spiky's 80ms: each label's own", p99)
	}
}

func TestLabelsKeepNoWaitDistributionWithoutPhases(t *testing.T) {
	srv, _ := recordingServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Requests = labelled("/a", "/b")
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.Labels {
		if l.Wait != nil {
			t.Errorf("%s has a wait distribution although phases were not asked for", l.Label)
		}
	}
}
