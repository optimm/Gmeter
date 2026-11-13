package report

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestASummarySaysHowManyAgentsSentTheLoad(t *testing.T) {
	s := Summarize(sampleResult(), 10)
	s.Run.Agents = 3
	var buf bytes.Buffer
	if err := s.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := ReadSummary(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if back.Run.Agents != 3 {
		t.Errorf("agents = %d after a round trip, want 3", back.Run.Agents)
	}

	s.Run.Agents = 0 // one machine, as every file from before agents
	buf.Reset()
	if err := s.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"agents"`) {
		t.Errorf("a run on one machine saves an agent count:\n%s", buf.String())
	}
}

func TestDiffSaysWhenTheLoadCameFromDifferentGenerators(t *testing.T) {
	a, b := mkSummary(100*time.Millisecond, 2), mkSummary(100*time.Millisecond, 2)
	b.Run.Agents = 3
	c, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "load generators differ (A: this machine, B: 3 agents)") {
		t.Errorf("notes = %q, want one saying the generators differ", c.Notes)
	}
	a.Run.Agents = 3
	if c, _ := Compare(a, b); strings.Contains(strings.Join(c.Notes, "\n"), "generators") {
		t.Errorf("both on 3 agents, but notes = %q", c.Notes)
	}
	a.Run.Agents, b.Run.Agents = 1, 2
	if c, _ := Compare(a, b); !strings.Contains(strings.Join(c.Notes, "\n"), "(A: 1 agent, B: 2 agents)") {
		t.Errorf("notes = %q, want 1 agent against 2", c.Notes)
	}
}
