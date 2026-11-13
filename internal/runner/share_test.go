package runner

import (
	"context"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
)

func TestAPhaseDelaysAnEvenSchedule(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration, cfg.Phase = 1, time.Second, 0.9 // the only request is due at 900ms
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 1 || res.Elapsed < 900*time.Millisecond {
		t.Errorf("%d scheduled, finished after %v; want 1, after 900ms", res.Scheduled, res.Elapsed)
	}
}

func TestASpanStretchesARecordedReplay(t *testing.T) {
	srv := okServer(t)
	cfg := baseConfig(srv.URL)
	cfg.Rate, cfg.Duration = 0, 0
	cfg.Offsets, cfg.Span = []time.Duration{0, 100 * time.Millisecond}, time.Second
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st := res.Stages[0].Stage; st.Duration != time.Second || st.From != 2 || st.To != 2 {
		t.Errorf("stage %v, want 1s at 2 rps: the replay is a second long, whatever its last request", st)
	}
	if res.Scheduled != 2 || res.Elapsed > 900*time.Millisecond {
		t.Errorf("%d scheduled, finished after %v; want 2, without waiting out the span", res.Scheduled, res.Elapsed)
	}
}

func TestPhaseAndSpanOnlyWhereTheyMeanSomething(t *testing.T) {
	offsets := func(c *Config) { c.Rate, c.Duration, c.Offsets = 0, 0, []time.Duration{0, time.Second} }
	bad := map[string]func(*Config){
		"phase of 1":               func(c *Config) { c.Phase = 1 },
		"negative phase":           func(c *Config) { c.Phase = -0.5 },
		"phase of random arrivals": func(c *Config) { c.Arrivals, c.Seed, c.Phase = pacer.Poisson, 1, 0.5 },
		"phase of a replay":        func(c *Config) { offsets(c); c.Phase = 0.5 },
		"span without offsets":     func(c *Config) { c.Span = time.Second },
		"span before the end":      func(c *Config) { offsets(c); c.Span = 500 * time.Millisecond },
		"negative span":            func(c *Config) { offsets(c); c.Span = -time.Second },
	}
	for name, spoil := range bad {
		c := baseConfig("http://127.0.0.1:1/")
		spoil(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := baseConfig("http://127.0.0.1:1/")
	offsets(&good)
	good.Span = time.Second // as long as the last offset is fine
	if err := good.Validate(); err != nil {
		t.Errorf("a span equal to the last offset: %v", err)
	}
}
