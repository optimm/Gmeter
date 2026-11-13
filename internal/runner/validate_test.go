package runner

import (
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
)

func TestValidateRefusesWhatRunWouldRefuse(t *testing.T) {
	good := baseConfig("http://127.0.0.1:1/")
	if err := good.Validate(); err != nil {
		t.Fatalf("a good config: %v", err)
	}
	bad := map[string]func(*Config){
		"no workers":        func(c *Config) { c.Workers = 0 },
		"not http":          func(c *Config) { c.URL = "ftp://x/" },
		"rate and stages":   func(c *Config) { c.Stages = pacer.ConstantRate(c.Rate, c.Duration) },
		"negative timeout":  func(c *Config) { c.Timeout = -time.Second },
		"target without /":  func(c *Config) { c.Requests = []Request{{Target: "x"}} },
		"offsets and rate":  func(c *Config) { c.Offsets = []time.Duration{0, time.Second} },
		"no time to run in": func(c *Config) { c.Duration = 0 },
	}
	for name, spoil := range bad {
		c := baseConfig("http://127.0.0.1:1/")
		spoil(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}
