package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// savedRun runs 30 requests against a server, saving them, and returns the file.
func resultsFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.csv")
	if code, _, stderr := exec(t, append(quickRun, "-results", path, okServer(t).URL)...); code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	return path
}

func TestPlotDrawsAResultsFileAsOnePage(t *testing.T) {
	csv := resultsFile(t)
	out := filepath.Join(t.TempDir(), "chart.html")

	code, stdout, stderr := exec(t, "plot", "-o", out, "-title", "checkout, before the fix", csv)

	if code != 0 || stdout != "" {
		t.Fatalf("exit code %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
	page, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), `"total":30`) || !strings.Contains(string(page), "checkout, before the fix") {
		t.Errorf("the page lacks the run's 30 requests or the title:\n%.300s", page)
	}
	if !strings.Contains(stderr, out) {
		t.Errorf("stderr does not say where the page went:\n%s", stderr)
	}
}

func TestPlotWritesNextToTheResultsFileUnlessToldOtherwise(t *testing.T) {
	csv := resultsFile(t)

	if code, _, stderr := exec(t, "plot", csv); code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}

	want := strings.TrimSuffix(csv, ".csv") + ".html"
	if _, err := os.Stat(want); err != nil {
		t.Errorf("no page at %s: %v", want, err)
	}
}

func TestPlotCanWriteThePageToStdout(t *testing.T) {
	code, stdout, stderr := exec(t, "plot", "-o", "-", resultsFile(t))

	if code != 0 || !strings.HasPrefix(stdout, "<!doctype html>") {
		t.Errorf("exit code %d, stdout starts %.40q, stderr:\n%s", code, stdout, stderr)
	}
}

func TestBadPlotUsageIsAnError(t *testing.T) {
	csv := resultsFile(t)
	notResults := writeTrace(t, "notes.txt", "hello\n")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no file":       {[]string{"plot"}, "results file"},
		"two files":     {[]string{"plot", csv, csv}, "exactly one"},
		"missing":       {[]string{"plot", filepath.Join(t.TempDir(), "nope.csv")}, "no such file"},
		"not a results": {[]string{"plot", notResults}, "not a gmeter results file"},
		"bad buckets":   {[]string{"plot", "-buckets", "0", csv}, "-buckets"},
		"cannot write":  {[]string{"plot", "-o", filepath.Join(t.TempDir(), "no", "x.html"), csv}, "no such file"},
	} {
		code, _, stderr := exec(t, tc.args...)
		if code != 2 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit code %d, stderr %q, want 2 and a message with %q", name, code, stderr, tc.want)
		}
	}
}
