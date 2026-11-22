package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrometheusWritesMetricsToAFileAndStillPrintsTheReport(t *testing.T) {
	srv := okServer(t)
	path := filepath.Join(t.TempDir(), "metrics.prom")

	code, stdout, stderr := exec(t, append(quickRun, "-prometheus", path, srv.URL)...)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "latency, corrected") {
		t.Errorf("the text report was not printed:\n%s", stdout)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "gmeter_scheduled_total") {
		t.Errorf("the metrics file does not look like Prometheus output:\n%s", data)
	}
}

func TestPrometheusDashPrintsOnlyMetrics(t *testing.T) {
	srv := okServer(t)

	code, stdout, stderr := exec(t, append(quickRun, "-prometheus", "-", srv.URL)...)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "# TYPE gmeter_scheduled_total counter") {
		t.Errorf("stdout is not Prometheus output:\n%s", stdout)
	}
	if strings.Contains(stdout, "latency, corrected") {
		t.Errorf("-prometheus - also printed the text report:\n%s", stdout)
	}
}

func TestJSONAndPrometheusFilesAreBothWritten(t *testing.T) {
	srv := okServer(t)
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "run.json")
	promPath := filepath.Join(dir, "run.prom")

	code, _, stderr := exec(t, append(quickRun, "-json", jsonPath, "-prometheus", promPath, srv.URL)...)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	for _, path := range []string{jsonPath, promPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not written: %v", path, err)
		}
	}
}

// A "-" for one format must not drop the other format's file: emit used to
// return as soon as it had written stdout.
func TestJSONOnStdoutStillWritesThePrometheusFile(t *testing.T) {
	srv := okServer(t)
	promPath := filepath.Join(t.TempDir(), "run.prom")

	code, stdout, stderr := exec(t, append(quickRun, "-json", "-", "-prometheus", promPath, srv.URL)...)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("stdout is not JSON:\n%s", stdout)
	}
	if _, err := os.Stat(promPath); err != nil {
		t.Fatalf("the Prometheus file was not written: %v", err)
	}
}

func TestPrometheusOnStdoutStillWritesTheJSONFile(t *testing.T) {
	srv := okServer(t)
	jsonPath := filepath.Join(t.TempDir(), "run.json")

	code, stdout, stderr := exec(t, append(quickRun, "-prometheus", "-", "-json", jsonPath, srv.URL)...)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "gmeter_scheduled_total") {
		t.Errorf("stdout is not Prometheus output:\n%s", stdout)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("the JSON file was not written: %v", err)
	}
}

func TestBothFormatsOnStdoutAreRefused(t *testing.T) {
	srv := okServer(t)

	code, _, stderr := exec(t, append(quickRun, "-json", "-", "-prometheus", "-", srv.URL)...)

	if code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr, "stdout") {
		t.Errorf("stderr does not explain the conflict:\n%s", stderr)
	}
}

// find has no metrics to export, so it must not offer a flag that does nothing.
func TestFindDoesNotOfferPrometheus(t *testing.T) {
	srv := okServer(t)

	code, _, stderr := exec(t, "find", "-slo", "p99=50ms", "-prometheus", "metrics.prom", srv.URL)

	if code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr, "flag provided but not defined: -prometheus") {
		t.Errorf("stderr does not report the unknown flag:\n%s", stderr)
	}
}
