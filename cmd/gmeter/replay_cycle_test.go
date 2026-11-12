package main

import (
	"strings"
	"testing"
)

// A trace shorter than the run is sent over and over, and every pass after the
// first is served from warmer caches: the report has to say so.
func TestReplayWarnsWhenAShortTraceIsCycled(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace) // 3 requests

	code, stdout, stderr := execReplay(t, "-file", trace, "-methods", "GET,POST", "-quiet", "-arrivals", "even", "-rate", "40", "-duration", "1s", srv.URL)

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"the trace has 3 requests but the run sent 40", "13.3 times", "warmer caches"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
}

func TestReplayDoesNotWarnWhenEveryRequestIsSentOnce(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)

	// Even arrivals: with Poisson the number sent in a second is random, and a
	// run that happens to send 4 of a 3-request trace has cycled it.
	for name, args := range map[string][]string{
		"recorded timing":              {},
		"a fixed rate, one pass":       {"-arrivals", "even", "-rate", "30"},
		"a fixed rate, a shorter run":  {"-arrivals", "even", "-rate", "2", "-duration", "1s"}, // 2 of the 3
		"a fixed rate, exactly a pass": {"-arrivals", "even", "-rate", "3", "-duration", "1s"},
	} {
		code, stdout, stderr := execReplay(t, append(append([]string{"-file", trace, "-methods", "GET,POST", "-quiet"}, args...), srv.URL)...)
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr:\n%s", name, code, stderr)
		}
		if strings.Contains(stdout, "warmer caches") {
			t.Errorf("%s: warned of a cycled trace although each request was sent at most once:\n%s", name, stdout)
		}
	}
}
