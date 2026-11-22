package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/journey"
)

// writeScenario writes a scenario file into a fresh scenario directory and
// returns the directory.
func writeScenario(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkout.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const simpleScenario = `name: checkout
steps:
  - name: sign-in
    path: /signin
    method: GET
    check:
      status: 200
  - name: cart
    path: /cart
    think: 1ms
    check:
      status: 200
`

func TestLoadTestJourneyRenderedAbsoluteURLIsRenderFailure(t *testing.T) {
	var secondHits atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(200)
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, second.URL+"/x")
	}))
	defer first.Close()

	scenario := `name: bypass
steps:
  - name: first
    path: /first
    extract:
      next: {from: regex, path: '(.+)', group: 1}
  - name: second
    path: '{{.next}}'
`
	dir := writeScenario(t, scenario)
	cs := connect(t, Options{ScenarioDir: dir})

	var got LoadTestResult
	e := call(t, cs, "load_test", map[string]any{
		"url":     first.URL,
		"journey": map[string]any{"file": "checkout.yaml", "rate": 5, "duration": "300ms"},
	}, &got)
	if e != "" {
		t.Fatal(e)
	}
	if secondHits.Load() != 0 {
		t.Fatalf("second server got %d requests, want 0", secondHits.Load())
	}
	if got.Journey == nil || got.Journey.Failures["render"] == 0 {
		t.Fatalf("want render failure, got %+v", got.Journey)
	}
}

func TestLoadTestRunsAJourney(t *testing.T) {
	dir := writeScenario(t, simpleScenario)
	url := target(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	cs := connect(t, Options{ScenarioDir: dir})

	var got LoadTestResult
	e := call(t, cs, "load_test", map[string]any{
		"url":      url,
		"journey":  map[string]any{"file": "checkout.yaml", "rate": 5, "duration": "500ms"},
	}, &got)
	if e != "" {
		t.Fatal(e)
	}
	skipIfLimited(t, got)
	if got.Verdict != "ok" || got.Journey == nil {
		t.Fatalf("verdict %q journey %+v: %s", got.Verdict, got.Journey, got.Summary)
	}
	if got.Journey.Steps != 2 || got.Journey.SessionsStarted == 0 || got.Journey.SessionsCompleted == 0 {
		t.Errorf("journey: %+v", got.Journey)
	}
	if !strings.Contains(got.Summary, "session") {
		t.Errorf("summary %q should say sessions: %s", got.Summary, got.Summary)
	}
}

func TestLoadTestJourneyRefusals(t *testing.T) {
	dir := writeScenario(t, simpleScenario)
	s := &service{policy: Policy{MaxRate: 1000, MaxDuration: time.Minute, MaxWorkers: 10}, scenarioDir: dir}
	ok := &JourneyInput{File: "checkout.yaml", Rate: 5, Duration: "500ms"}
	if _, _, err := s.compileJourney(context.Background(), ok, 10); err != nil {
		t.Fatalf("compileJourney: %v", err)
	}

	sNoDir := &service{policy: Policy{MaxRate: 1000, MaxWorkers: 10}}
	if _, _, err := sNoDir.compileJourney(context.Background(), ok, 10); err == nil || !strings.Contains(err.Error(), "scenario-dir") {
		t.Errorf("no scenario dir: got %v", err)
	}
	if _, _, err := s.compileJourney(context.Background(), &JourneyInput{File: "../escape.yaml", Rate: 5}, 10); err == nil || !strings.Contains(err.Error(), "not in the scenario directory") {
		t.Errorf("escaping file: got %v", err)
	}
	if _, _, err := s.compileJourney(context.Background(), &JourneyInput{File: "checkout.yaml", Rate: 0}, 10); err == nil {
		t.Error("rate 0 accepted")
	}

	// The effective request rate is the session rate times the step count:
	// 5 sessions/s at 2 steps is 10/s, within MaxRate 12; 3 steps at 5/s
	// would be 15/s, over it.
	s2 := &service{policy: Policy{MaxRate: 12, MaxDuration: time.Minute, MaxWorkers: 10}, scenarioDir: dir}
	if _, _, err := s2.compileJourney(context.Background(), ok, 10); err != nil {
		t.Errorf("10 effective requests/s within MaxRate 12: got %v", err)
	}
	s3 := &service{policy: Policy{MaxRate: 9, MaxDuration: time.Minute, MaxWorkers: 10}, scenarioDir: dir}
	if _, _, err := s3.compileJourney(context.Background(), ok, 10); err == nil || !strings.Contains(err.Error(), "over this server's limit") {
		t.Errorf("10 effective requests/s over MaxRate 9: got %v", err)
	}

	// A mutating step is refused by name of the setting; it runs with it on.
	dirW := writeScenario(t, "name: w\nsteps:\n  - name: post\n    path: /x\n    method: POST\n")
	sW := &service{policy: Policy{MaxRate: 1000, MaxWorkers: 10}, scenarioDir: dirW}
	if _, _, err := sW.compileJourney(context.Background(), &JourneyInput{File: "checkout.yaml", Rate: 5}, 10); err == nil || !strings.Contains(err.Error(), "AllowWriteScenarios") {
		t.Errorf("write step without the setting: got %v", err)
	}
	sW.policy.AllowWriteScenarios = true
	if _, _, err := sW.compileJourney(context.Background(), &JourneyInput{File: "checkout.yaml", Rate: 5}, 10); err != nil {
		t.Errorf("write step with the setting: got %v", err)
	}
}

func TestJourneyLoadedByName(t *testing.T) {
	dir := writeScenario(t, simpleScenario)
	if _, err := journey.LoadNamed(dir, "checkout.yaml", false); err != nil {
		t.Fatal(err)
	}
	if _, err := journey.LoadNamed(dir, "nope.yaml", false); err == nil {
		t.Error("a missing scenario name was accepted")
	}
	if _, err := journey.LoadNamed(dir, "../escape.yaml", false); err == nil {
		t.Error("an escaping scenario name was accepted")
	}
	if _, err := journey.LoadNamed(dir, filepath.Join(dir, "checkout.yaml"), false); err == nil {
		t.Error("an absolute scenario name was accepted without AllowRawJourneyYAML")
	}
	if _, err := journey.LoadNamed(dir, filepath.Join(dir, "checkout.yaml"), true); err != nil {
		t.Errorf("AllowRawJourneyYAML: got %v", err)
	}
}
