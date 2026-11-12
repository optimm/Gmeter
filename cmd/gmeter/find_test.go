package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// capacityServer does `work` of work per request, `slots` at a time, so it
// serves about slots/work requests a second and queues beyond that.
func capacityServer(t *testing.T, slots int, work time.Duration) *httptest.Server {
	t.Helper()
	sem := make(chan struct{}, slots)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sem <- struct{}{}
		time.Sleep(work)
		<-sem
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fast(t *testing.T) {
	t.Helper()
	old := settle
	settle = 0
	t.Cleanup(func() { settle = old })
}

type foundJSON struct {
	Result struct {
		Found       bool    `json:"found"`
		MaxRPS      float64 `json:"max_rps"`
		NextFailRPS float64 `json:"next_fail_rps"`
		Interrupted bool    `json:"interrupted"`
	} `json:"result"`
	Run struct {
		URL      string `json:"url"`
		Arrivals string `json:"arrivals"`
		Seed     uint64 `json:"seed"`
	} `json:"run"`
	Trials []struct {
		Phase   string  `json:"phase"`
		RPS     float64 `json:"rps"`
		Verdict string  `json:"verdict"`
	} `json:"trials"`
}

func findArgs(url string, extra ...string) []string {
	args := []string{"find", "-slo", "p99=60ms", "-trial", "400ms", "-warmup", "100ms", "-workers", "100", "-arrivals", "even"}
	return append(append(args, extra...), url)
}

func TestFindLocatesTheCapacityOfAServer(t *testing.T) {
	fast(t)
	srv := capacityServer(t, 2, 10*time.Millisecond) // about 200 rps
	path := filepath.Join(t.TempDir(), "find.json")

	code, out, stderr := exec(t, findArgs(srv.URL, "-start", "50", "-max", "1000", "-tolerance", "20", "-confirm", "1", "-json", path)...)
	if code != 0 {
		t.Fatalf("exit %d, stdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	if !strings.Contains(out, "the highest rate meeting p99 ≤ 60ms is about") {
		t.Errorf("the report should state the answer:\n%s", out)
	}

	raw, _ := os.ReadFile(path)
	var got foundJSON
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// Nominal capacity is 200 rps; allow for timing noise on a busy machine.
	if !got.Result.Found || got.Result.MaxRPS < 50 || got.Result.MaxRPS > 300 {
		t.Errorf("max rate %v, want somewhere around 200 (50..300): %s", got.Result.MaxRPS, raw)
	}
	if got.Result.NextFailRPS <= got.Result.MaxRPS {
		t.Errorf("the failing rate (%v) must be above the passing one (%v)", got.Result.NextFailRPS, got.Result.MaxRPS)
	}
	if len(got.Trials) < 4 || got.Trials[0].Phase != "grow" || got.Trials[0].RPS != 50 {
		t.Errorf("trials should start by growing from 50: %+v", got.Trials)
	}
	if !strings.Contains(stderr, "trial 1") {
		t.Errorf("progress should name each trial on stderr:\n%s", stderr)
	}
}

func TestFindExitsOneWhenNoRateMeetsTheTarget(t *testing.T) {
	fast(t)
	srv := capacityServer(t, 100, 100*time.Millisecond) // every request takes 100ms: p99=10ms is unreachable
	code, out, _ := exec(t, "find", "-slo", "p99=10ms", "-start", "8", "-max", "100", "-trial", "300ms", "-warmup", "0",
		"-confirm", "0", "-arrivals", "even", "-quiet", srv.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "no rate down to 1 rps met p99 ≤ 10ms") {
		t.Errorf("the report should say nothing met the target:\n%s", out)
	}
}

func TestFindJSONDashWritesOnlyJSON(t *testing.T) {
	fast(t)
	srv := okServer(t)
	code, out, _ := exec(t, "find", "-slo", "p99=1s", "-start", "20", "-max", "20", "-trial", "300ms", "-warmup", "0",
		"-confirm", "0", "-arrivals", "even", "-quiet", "-json", "-", srv.URL)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var got foundJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout should be only JSON: %v\n%s", err, out)
	}
	if !got.Result.Found || got.Result.MaxRPS != 20 {
		t.Errorf("start=max=20 against a fast server should find 20: %+v", got.Result)
	}
}

func TestFindNeverPrintsOrSavesACredential(t *testing.T) {
	fast(t)
	srv := okServer(t)
	withCreds := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1)
	path := filepath.Join(t.TempDir(), "find.json")
	code, out, stderr := exec(t, "find", "-slo", "p99=1s", "-start", "20", "-max", "20", "-trial", "300ms", "-warmup", "0",
		"-confirm", "0", "-arrivals", "even", "-H", "Authorization: Bearer topsecret", "-json", path, withCreds)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	raw, _ := os.ReadFile(path)
	for name, text := range map[string]string{"stdout": out, "stderr": stderr, "saved file": string(raw)} {
		for _, secret := range []string{"s3cret", "topsecret"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks %q:\n%s", name, secret, text)
			}
		}
	}
}

func TestFindReportsAnInterruptedSearchAndExitsOne(t *testing.T) {
	fast(t)
	srv := capacityServer(t, 2, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var out, errOut strings.Builder
	code := run(ctx, findArgs(srv.URL, "-start", "20", "-max", "1000", "-quiet"), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1 for an interrupted search:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "interrupted") {
		t.Errorf("the report should say it was interrupted:\n%s", out.String())
	}
}

func TestFindUsageErrorsExitWithCode2(t *testing.T) {
	srv := okServer(t)
	cases := map[string]struct {
		args    []string
		message string
	}{
		"no target":          {[]string{"find", srv.URL}, "-slo"},
		"malformed target":   {[]string{"find", "-slo", "p99", srv.URL}, "p99=50ms"},
		"no url":             {[]string{"find", "-slo", "p99=50ms"}, "URL"},
		"zero start":         {[]string{"find", "-slo", "p99=50ms", "-start", "0", srv.URL}, "starting rate"},
		"max below start":    {[]string{"find", "-slo", "p99=50ms", "-start", "500", "-max", "100", srv.URL}, "maximum"},
		"zero tolerance":     {[]string{"find", "-slo", "p99=50ms", "-tolerance", "0", srv.URL}, "tolerance"},
		"tolerance too big":  {[]string{"find", "-slo", "p99=50ms", "-tolerance", "150", srv.URL}, "tolerance"},
		"negative errors":    {[]string{"find", "-slo", "p99=50ms", "-max-errors", "-1", srv.URL}, "-max-errors"},
		"errors over 100":    {[]string{"find", "-slo", "p99=50ms", "-max-errors", "101", srv.URL}, "-max-errors"},
		"zero trial":         {[]string{"find", "-slo", "p99=50ms", "-trial", "0s", srv.URL}, "-trial"},
		"negative warmup":    {[]string{"find", "-slo", "p99=50ms", "-warmup", "-1s", srv.URL}, "-warmup"},
		"negative confirm":   {[]string{"find", "-slo", "p99=50ms", "-confirm", "-1", srv.URL}, "confirm"},
		"unknown arrivals":   {[]string{"find", "-slo", "p99=50ms", "-arrivals", "random", srv.URL}, "poisson"},
		"bad url":            {[]string{"find", "-slo", "p99=50ms", "-trial", "200ms", "ftp://nope"}, "url"},
		"flag after the url": {[]string{"find", srv.URL, "-slo", "p99=50ms"}, "before the URL"},
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

// Every progress line, and the usage text, must end in a real newline.
func TestFindOutputUsesRealNewlines(t *testing.T) {
	fast(t)
	srv := okServer(t)
	_, _, stderr := exec(t, "find", "-slo", "p99=1s", "-start", "20", "-max", "40", "-trial", "300ms", "-warmup", "0",
		"-confirm", "0", "-arrivals", "even", srv.URL)
	if strings.Contains(stderr, `\n`) {
		t.Errorf("progress contains a literal backslash-n:\n%s", stderr)
	}
	if lines := strings.Split(strings.TrimSpace(stderr), "\n"); len(lines) < 3 { // banner + two trials
		t.Errorf("want the banner and a line per trial, got %d lines:\n%s", len(lines), stderr)
	}

	_, _, usage := exec(t, "find")
	if strings.Contains(usage, `\n`) || !strings.Contains(usage, "Usage: gmeter find") {
		t.Errorf("the usage text should be real lines:\n%s", usage)
	}
}
