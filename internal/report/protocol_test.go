package report

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestTheReportSaysWhichProtocolTheResponsesCameOver(t *testing.T) {
	res := sampleResult()
	res.Protocols = map[string]uint64{"HTTP/2.0": 90, "HTTP/1.1": 10}
	s := Summarize(res, 10)
	var buf strings.Builder
	if err := s.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "protocol   HTTP/1.1: 10   HTTP/2.0: 90\n") {
		t.Errorf("no protocol line:\n%s", buf.String())
	}
	var js bytes.Buffer
	if err := s.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"HTTP/2.0": 90`) {
		t.Errorf("the JSON has no protocols:\n%s", js.String())
	}
}

func TestProtocolErrorsAreExplained(t *testing.T) {
	res := sampleResult()
	res.Errors = map[string]uint64{"protocol": 7}
	s := Summarize(res, 10)
	found := false
	for _, w := range s.Warnings {
		if strings.Contains(w, "7 of") && strings.Contains(w, "HTTP/2") && strings.Contains(w, "-http2") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning explaining the protocol errors: %q", s.Warnings)
	}
}

func TestDiffSaysWhenTheRunsUsedDifferentProtocols(t *testing.T) {
	a, b := mkSummary(100*time.Millisecond, 2), mkSummary(100*time.Millisecond, 2)
	b.Run.Protocol = "HTTP/2"
	c, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "protocol differs (A: HTTP/1.1, B: HTTP/2)") {
		t.Errorf("notes = %q, want one saying the protocols differ", c.Notes)
	}
	a.Run.Protocol = "HTTP/2"
	if c, _ := Compare(a, b); strings.Contains(strings.Join(c.Notes, "\n"), "protocol") {
		t.Errorf("both HTTP/2, but notes = %q", c.Notes)
	}
}
