package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// exec runs the CLI in-process and returns its exit code and output.
func exec(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestSuccessfulRunPrintsAReport(t *testing.T) {
	srv := okServer(t)

	code, stdout, stderr := exec(t, "-rate", "100", "-duration", "300ms", "-workers", "4", "-quiet", srv.URL)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"30 scheduled", "30 ok", "p99", "corrected", "service"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("-quiet run wrote to stderr: %q", stderr)
	}
}

func TestJSONFlagWritesSummaryToAFile(t *testing.T) {
	srv := okServer(t)
	path := filepath.Join(t.TempDir(), "run.json")

	code, stdout, stderr := exec(t, "-rate", "100", "-duration", "300ms", "-quiet", "-json", path, srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "p99") {
		t.Errorf("the text report should still be printed when -json writes a file:\n%s", stdout)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Scheduled uint64 `json:"scheduled"`
		TargetRPS int    `json:"target_rps"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	if got.Scheduled != 30 || got.TargetRPS != 100 {
		t.Fatalf("json = %+v, want scheduled=30 target_rps=100", got)
	}
}

func TestJSONDashWritesOnlyJSONToStdout(t *testing.T) {
	srv := okServer(t)

	code, stdout, stderr := exec(t, "-rate", "100", "-duration", "200ms", "-quiet", "-json", "-", srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("stdout must be pure JSON with -json -, got: %v\n%s", err, stdout)
	}
}

func TestProgressGoesToStderrUnlessQuiet(t *testing.T) {
	old := sampleEvery
	sampleEvery = 100 * time.Millisecond
	t.Cleanup(func() { sampleEvery = old })
	srv := okServer(t)

	_, _, stderr := exec(t, "-rate", "50", "-duration", "450ms", srv.URL)

	if !strings.Contains(stderr, "scheduled") {
		t.Fatalf("expected progress lines on stderr, got %q", stderr)
	}
}

func TestSendsMethodHeadersAndBodyFromFile(t *testing.T) {
	var mu sync.Mutex
	var method, h1, h2, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		method, h1, h2, body = r.Method, r.Header.Get("X-One"), r.Header.Get("Authorization"), string(b)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	bodyFile := filepath.Join(t.TempDir(), "order.json")
	if err := os.WriteFile(bodyFile, []byte(`{"sku":42}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := exec(t, "-rate", "20", "-duration", "100ms", "-quiet",
		"-method", "POST", "-H", "X-One: first", "-H", "Authorization: Bearer t0k3n",
		"-body", "@"+bodyFile, srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if method != "POST" || h1 != "first" || h2 != "Bearer t0k3n" || body != `{"sku":42}` {
		t.Fatalf("server saw method=%q X-One=%q Authorization=%q body=%q", method, h1, h2, body)
	}
}

func TestUsageErrorsExitWithCode2(t *testing.T) {
	srv := okServer(t)
	cases := map[string]struct {
		args    []string
		message string
	}{
		"no url":               {[]string{"-rate", "10"}, "URL"},
		"flags after the url":  {[]string{srv.URL, "-rate", "10"}, "before the URL"},
		"header without colon": {[]string{"-H", "nocolon", srv.URL}, "Key: Value"},
		"unreadable body file": {[]string{"-body", "@/definitely/not/here", srv.URL}, "body"},
		"zero rate":            {[]string{"-rate", "0", srv.URL}, "rate"},
		"unknown flag":         {[]string{"-nope", srv.URL}, "nope"},
		"negative spin":        {[]string{"-spin", "-1ms", srv.URL}, "must not be negative"},
		"stages with rate":     {[]string{"-stages", "1s@10", "-rate", "5", srv.URL}, "cannot be combined"},
		"stages with duration": {[]string{"-stages", "1s@10", "-duration", "5s", srv.URL}, "cannot be combined"},
		"malformed stages":     {[]string{"-stages", "soon@10", srv.URL}, "stage 1"},
		"stages of nothing":    {[]string{"-stages", "1s@0", srv.URL}, "no requests"},
		"unknown arrivals":     {[]string{"-arrivals", "random", srv.URL}, "poisson"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := exec(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit code %d, want 2; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tc.message) {
				t.Fatalf("stderr should mention %q:\n%s", tc.message, stderr)
			}
		})
	}
}

func TestHelpExitsZero(t *testing.T) {
	code, _, stderr := exec(t, "-h")
	if code != 0 {
		t.Fatalf("exit code %d, want 0 for -h", code)
	}
	if !strings.Contains(stderr, "-rate") {
		t.Fatalf("usage should list flags:\n%s", stderr)
	}
}

func TestProgressLineShowsTheLatestWindow(t *testing.T) {
	old := sampleEvery
	sampleEvery = 100 * time.Millisecond
	t.Cleanup(func() { sampleEvery = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	_, _, stderr := exec(t, "-rate", "100", "-duration", "450ms", srv.URL)

	re := regexp.MustCompile(`window: \d+ rps, p50 \S+ p99 \S+ max \S+`)
	if !re.MatchString(stderr) {
		t.Fatalf("progress should show the window's rps and percentiles, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "late") {
		t.Fatalf("progress should show the late count so saturation is visible live:\n%s", stderr)
	}
}

func slowServer(t *testing.T, d time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// saveRun runs the CLI against url and returns the path of the JSON it saved.
func saveRun(t *testing.T, url string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.json")
	if code, _, stderr := exec(t, "-rate", "100", "-duration", "300ms", "-quiet", "-json", path, url); code != 0 {
		t.Fatalf("run against %s exited %d: %s", url, code, stderr)
	}
	return path
}

func savedSpin(t *testing.T, args ...string) float64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.json")
	args = append([]string{"-rate", "50", "-duration", "200ms", "-quiet", "-json", path}, args...)
	if code, _, stderr := exec(t, args...); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	raw, _ := os.ReadFile(path)
	var got struct {
		Run struct {
			SpinSeconds float64 `json:"spin_seconds"`
		} `json:"run"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got.Run.SpinSeconds
}

// The spin window changes what the latencies mean, so it is saved, and the
// default (200µs) is the one that makes a plain run precise.
func TestSpinDefaultsToTwoHundredMicrosecondsAndCanBeSet(t *testing.T) {
	srv := okServer(t)
	if got := savedSpin(t, srv.URL); got != 0.0002 {
		t.Errorf("default spin saved as %v s, want 0.0002", got)
	}
	if got := savedSpin(t, "-spin", "1ms", srv.URL); got != 0.001 {
		t.Errorf("-spin 1ms saved as %v s, want 0.001", got)
	}
	if got := savedSpin(t, "-spin", "0", srv.URL); got != 0 {
		t.Errorf("-spin 0 saved as %v s, want 0 (plain timer)", got)
	}
}

func TestSavedJSONRecordsWhatWasRun(t *testing.T) {
	srv := okServer(t)
	path := filepath.Join(t.TempDir(), "run.json")

	if code, _, stderr := exec(t, "-rate", "100", "-duration", "300ms", "-workers", "4", "-quiet", "-json", path, srv.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	raw, _ := os.ReadFile(path)
	var got struct {
		Version int `json:"version"`
		Run     struct {
			URL             string  `json:"url"`
			Method          string  `json:"method"`
			Workers         int     `json:"workers"`
			DurationSeconds float64 `json:"duration_seconds"`
			StartedAt       string  `json:"started_at"`
		} `json:"run"`
		Timeline   []any `json:"timeline"`
		Histograms struct {
			Corrected struct {
				Buckets [][2]uint64 `json:"buckets"`
			} `json:"corrected"`
		} `json:"histograms"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.Run.URL != srv.URL || got.Run.Method != "GET" || got.Run.Workers != 4 || got.Run.DurationSeconds != 0.3 {
		t.Fatalf("run info wrong: %+v", got)
	}
	if _, err := time.Parse(time.RFC3339, got.Run.StartedAt); err != nil {
		t.Fatalf("started_at %q is not RFC 3339: %v", got.Run.StartedAt, err)
	}
	if len(got.Timeline) == 0 || len(got.Histograms.Corrected.Buckets) == 0 {
		t.Fatalf("timeline (%d) and histogram buckets (%d) must both be saved", len(got.Timeline), len(got.Histograms.Corrected.Buckets))
	}
}

func TestCredentialsNeverReachTheOutputsOrTheSavedFile(t *testing.T) {
	srv := okServer(t)
	withCreds := strings.Replace(srv.URL, "http://", "http://alice:s3cret@", 1)
	path := filepath.Join(t.TempDir(), "run.json")

	code, stdout, stderr := exec(t, "-rate", "50", "-duration", "100ms", "-json", path,
		"-H", "Authorization: Bearer topsecret", withCreds) // not -quiet: the banner is printed
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	raw, _ := os.ReadFile(path)

	for name, out := range map[string]string{"stdout": stdout, "stderr": stderr, "saved JSON": string(raw)} {
		for _, secret := range []string{"s3cret", "topsecret"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s leaks %q:\n%s", name, secret, out)
			}
		}
	}
	if !strings.Contains(string(raw), "alice:xxxxx@") {
		t.Errorf("saved URL should keep the user but redact the password:\n%s", raw)
	}
}

func TestDiffComparesTwoSavedRuns(t *testing.T) {
	fast := saveRun(t, okServer(t).URL)
	slow := saveRun(t, slowServer(t, 30*time.Millisecond).URL)

	code, stdout, stderr := exec(t, "diff", fast, slow)

	if code != 0 {
		t.Fatalf("exit %d without a threshold, want 0; stderr: %s", code, stderr)
	}
	for _, want := range []string{"p99", "change", "slower than A's p99", fast, slow} {
		if !strings.Contains(stdout, want) {
			t.Errorf("diff output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestDiffThresholdFailsOnARegressionAndPassesOnAnImprovement(t *testing.T) {
	fast := saveRun(t, okServer(t).URL)
	slow := saveRun(t, slowServer(t, 30*time.Millisecond).URL)

	code, stdout, _ := exec(t, "diff", "-threshold", "20", fast, slow)
	if code != 1 || !strings.Contains(stdout, "REGRESSION") {
		t.Fatalf("fast -> slow with -threshold 20: exit %d, want 1 and a REGRESSION line:\n%s", code, stdout)
	}

	code, stdout, _ = exec(t, "diff", "-threshold", "20", slow, fast)
	if code != 0 || !strings.Contains(stdout, "ok") {
		t.Fatalf("slow -> fast with -threshold 20: exit %d, want 0:\n%s", code, stdout)
	}
}

func TestDiffUsageErrorsExitWithCode2(t *testing.T) {
	good := saveRun(t, okServer(t).URL)
	notJSON := filepath.Join(t.TempDir(), "junk.json")
	if err := os.WriteFile(notJSON, []byte("this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		args    []string
		message string
	}{
		"no files":      {[]string{"diff"}, "two"},
		"one file":      {[]string{"diff", good}, "two"},
		"three files":   {[]string{"diff", good, good, good}, "two"},
		"missing file":  {[]string{"diff", good, "/definitely/not/here.json"}, "not/here.json"},
		"not a summary": {[]string{"diff", good, notJSON}, "junk.json"},
		"bad threshold": {[]string{"diff", "-threshold", "lots", good, good}, "threshold"},
		"unknown flag":  {[]string{"diff", "-nope", good, good}, "nope"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := exec(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tc.message) {
				t.Fatalf("stderr should mention %q:\n%s", tc.message, stderr)
			}
		})
	}
}

type savedRun struct {
	Run struct {
		DurationSeconds float64 `json:"duration_seconds"`
		Profile         string  `json:"profile"`
		Arrivals        string  `json:"arrivals"`
		Seed            uint64  `json:"seed"`
	} `json:"run"`
	TargetRPS float64 `json:"target_rps"`
	Scheduled uint64  `json:"scheduled"`
	Stages    []struct {
		Profile   string `json:"profile"`
		Scheduled uint64 `json:"scheduled"`
	} `json:"stages"`
}

func runAndLoad(t *testing.T, args ...string) (saved savedRun, stdout, stderr string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.json")
	code, out, errOut := exec(t, append([]string{"-json", path}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	return saved, out, errOut
}

func TestStagesFlagRunsAStepProfileAndReportsEachStage(t *testing.T) {
	srv := okServer(t)
	saved, out, _ := runAndLoad(t, "-quiet", "-stages", "300ms@50,300ms@150", srv.URL)

	if saved.Scheduled != 60 { // 15 + 45
		t.Errorf("scheduled %d, want 60", saved.Scheduled)
	}
	if saved.Run.Profile != "300ms@50,300ms@150" || saved.Run.DurationSeconds != 0.6 {
		t.Errorf("saved profile %q over %vs, want 300ms@50,300ms@150 over 0.6s", saved.Run.Profile, saved.Run.DurationSeconds)
	}
	if saved.TargetRPS != 100 { // 60 requests over 0.6s
		t.Errorf("target_rps = %v, want the average, 100", saved.TargetRPS)
	}
	if len(saved.Stages) != 2 || saved.Stages[0].Scheduled != 15 || saved.Stages[1].Scheduled != 45 {
		t.Errorf("saved stages = %+v, want 15 and 45 scheduled", saved.Stages)
	}
	for _, want := range []string{"stages", "300ms@50", "300ms@150", "on average"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

func TestAConstantRateIsSavedAsAOneStageEvenProfile(t *testing.T) {
	srv := okServer(t)
	saved, out, _ := runAndLoad(t, "-quiet", "-rate", "50", "-duration", "300ms", srv.URL)

	if saved.Run.Profile != "300ms@50" || saved.Run.Arrivals != "even" || saved.Run.Seed != 0 {
		t.Errorf("saved %+v, want profile 300ms@50, even arrivals, no seed", saved.Run)
	}
	if len(saved.Stages) != 1 {
		t.Errorf("%d stages saved, want 1", len(saved.Stages))
	}
	if strings.Contains(out, "stages (") {
		t.Errorf("a constant rate should not print a stage table:\n%s", out)
	}
}

// The seed is what makes a random arrival pattern repeatable, so it is always
// printed and saved, including one Gmeter picked itself.
func TestPoissonArrivalsAreReproducibleFromTheSeedItReports(t *testing.T) {
	srv := okServer(t)
	args := []string{"-rate", "100", "-duration", "500ms", "-arrivals", "poisson"}

	first, _, stderr := runAndLoad(t, append(args, srv.URL)...) // no seed given: one is chosen
	m := regexp.MustCompile(`seed (\d+)`).FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("the banner should name the seed that was chosen:\n%s", stderr)
	}
	if first.Run.Arrivals != "poisson" || first.Run.Seed == 0 || strconv.FormatUint(first.Run.Seed, 10) != m[1] {
		t.Fatalf("saved arrivals/seed = %q/%d, want poisson and the printed seed %s", first.Run.Arrivals, first.Run.Seed, m[1])
	}

	again, _, _ := runAndLoad(t, append(append(args, "-quiet", "-seed", m[1]), srv.URL)...)
	if again.Scheduled != first.Scheduled || again.Run.Seed != first.Run.Seed {
		t.Fatalf("the same seed gave %d requests, then %d", first.Scheduled, again.Scheduled)
	}
}
