package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/report"
)

// journeyScenario writes a two-step scenario for tests and returns its path.
func journeyScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checkout.yaml")
	body := `name: checkout
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
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJourneyRunPrintsJourneyReport(t *testing.T) {
	srv := okServer(t)
	sc := journeyScenario(t)
	jsonOut := filepath.Join(t.TempDir(), "j.json")

	code, stdout, stderr := exec(t, "journey", "-scenario", sc, "-rate", "20", "-duration", "300ms", "-json", jsonOut, srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"sessions   ", "journey   ", "steps,"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	raw, err := os.ReadFile(jsonOut)
	if err != nil {
		t.Fatal(err)
	}
	s, err := report.ReadSummary(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if s.Journey == nil {
		t.Fatal("summary has no journey section")
	}
	if s.Journey.Name != "checkout" || s.Journey.Steps != 2 {
		t.Errorf("summary journey: %+v", s.Journey)
	}
	if s.Journey.SessionsStarted == 0 || s.Journey.SessionsCompleted == 0 {
		t.Errorf("summary journey counters: %+v", s.Journey)
	}
	if s.Run.Journey == nil {
		t.Error("RunInfo has no journey section")
	}
}

func TestJourneyUsageErrors(t *testing.T) {
	srv := okServer(t)
	sc := journeyScenario(t)
	for name, args := range map[string][]string{
		"missing URL":           {"journey", "-scenario", sc},
		"missing scenario file": {"journey", "-scenario", filepath.Join(t.TempDir(), "nope.yaml"), srv.URL},
		"unknown flag":          {"journey", "-scenario", sc, "-bogus", srv.URL},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, _ := exec(t, args...)
			if code != 2 {
				t.Errorf("exit code %d, want 2", code)
			}
		})
	}
	code, _, stderr := exec(t, "journey", "-scenario", sc, srv.URL)
	if code != 0 {
		t.Errorf("journey with a valid scenario: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = exec(t, "journey", srv.URL)
	if code != 2 {
		t.Errorf("no scenario flag: exit %d, stderr %q", code, stderr)
	}
}

func TestFindScenarioReportsSessionsPerSecond(t *testing.T) {
	srv := okServer(t)
	sc := journeyScenario(t)
	code, stdout, stderr := exec(t, "find", "-scenario", sc, "-slo", "p99=10ms", "-start", "20", "-max", "20",
		"-trial", "500ms", "-warmup", "0s", "-confirm", "1", "-quiet", srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "sessions/s") {
		t.Errorf("stdout is missing the sessions/s wording:\n%s", stdout)
	}
}

func TestFindScenarioExits1OnTargetMiss(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer slow.Close()
	sc := journeyScenario(t)
	// Even arrivals put a session due at the trial's start, so the trial
	// measures the slow server rather than the generator.
	code, _, stderr := exec(t, "find", "-scenario", sc, "-slo", "p99=1ms", "-start", "2", "-max", "5",
		"-trial", "500ms", "-warmup", "0s", "-confirm", "1", "-arrivals", "even", "-quiet", slow.URL)
	if code != 1 {
		t.Errorf("exit code %d, want 1; stderr:\n%s", code, stderr)
	}
}

func TestJourneyBasicsAgainstSyntheticServer(t *testing.T) {
	// A quick journey run against the shared helper server, checking exit 0 and
	// the report's journey line, with the steps recorded as the endpoints rows.
	srv := okServer(t)
	sc := journeyScenario(t)
	code, stdout, _ := exec(t, "journey", "-scenario", sc, "-rate", "10", "-duration", "200ms", "-quiet", srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "sign-in") || !strings.Contains(stdout, "cart") {
		t.Errorf("stdout is missing the step rows:\n%s", stdout)
	}
}

var _ = context.Background // silence unused if helpers change
