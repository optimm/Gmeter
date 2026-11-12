package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// served is what the server under test was asked for.
type served struct {
	method, uri, body string
	header            http.Header
}

func traceServer(t *testing.T) (*httptest.Server, func() []served) {
	t.Helper()
	var mu sync.Mutex
	var got []served
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, served{r.Method, r.RequestURI, string(b), r.Header.Clone()})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, func() []served {
		mu.Lock()
		defer mu.Unlock()
		return append([]served(nil), got...)
	}
}

func writeTrace(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func uris(got []served) []string {
	var out []string
	for _, s := range got {
		out = append(out, s.method+" "+s.uri)
	}
	sort.Strings(out)
	return out
}

const timedTrace = `{"at":0,"path":"/a"}
{"at":0.1,"method":"POST","path":"/b","body":"x=1","headers":{"X-K":"v"}}
{"at":0.2,"path":"/c?q=1"}
`

func TestReplayRecordedTimingSendsEachRequestAsRecorded(t *testing.T) {
	srv, got := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)

	code, stdout, stderr := execReplay(t, "-file", trace, "-methods", "GET,POST", "-workers", "4", srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"3 scheduled", "3 ok"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	reqs := got()
	if want := []string{"GET /a", "GET /c?q=1", "POST /b"}; strings.Join(uris(reqs), "|") != strings.Join(want, "|") {
		t.Errorf("server saw %v, want %v", uris(reqs), want)
	}
	for _, r := range reqs {
		if r.method == "POST" && (r.body != "x=1" || r.header.Get("X-K") != "v") {
			t.Errorf("POST /b arrived with body %q and X-K %q, want x=1 and v", r.body, r.header.Get("X-K"))
		}
	}
	if !strings.Contains(stderr, "replaying 3 requests from trace.jsonl") || !strings.Contains(stderr, "recorded timing") {
		t.Errorf("banner does not describe the replay:\n%s", stderr)
	}
	if strings.Contains(stderr, `\n`) {
		t.Errorf("stderr contains a literal backslash-n:\n%s", stderr)
	}
}

// execReplay runs `gmeter replay ...`.
func execReplay(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return exec(t, append([]string{"replay"}, args...)...)
}

func TestReplaySavesWhatWasReplayedInTheJSON(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)
	out := filepath.Join(t.TempDir(), "run.json")

	code, _, stderr := execReplay(t, "-file", trace, "-methods", "GET,POST", "-quiet", "-speed", "2", "-json", out, srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Run struct {
			Method string `json:"method"`
			Replay *struct {
				Source   string  `json:"source"`
				Format   string  `json:"format"`
				Requests int     `json:"requests"`
				Timing   string  `json:"timing"`
				Speed    float64 `json:"speed"`
			} `json:"replay"`
		} `json:"run"`
		Scheduled uint64 `json:"scheduled"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Run.Replay == nil {
		t.Fatalf("no replay block in the JSON:\n%s", raw)
	}
	r := got.Run.Replay
	if r.Source != "trace.jsonl" || r.Format != "jsonl" || r.Requests != 3 || r.Timing != "recorded" || r.Speed != 2 {
		t.Errorf("replay = %+v, want trace.jsonl, jsonl, 3 requests, recorded timing at speed 2", *r)
	}
	if got.Run.Method != "GET,POST" {
		t.Errorf("method = %q, want the methods actually in the trace: GET,POST", got.Run.Method)
	}
	if strings.Contains(string(raw), "x=1") || strings.Contains(string(raw), `"X-K"`) {
		t.Error("request bodies and headers must never be saved")
	}
}

func TestReplayWithAFixedRateCyclesThroughTheTrace(t *testing.T) {
	srv, got := traceServer(t)
	trace := writeTrace(t, "untimed.jsonl", `{"path":"/a"}`+"\n"+`{"path":"/b"}`+"\n"+`{"path":"/c"}`+"\n")

	code, stdout, stderr := execReplay(t, "-file", trace, "-rate", "100", "-duration", "300ms", "-arrivals", "even", "-quiet", srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "30 scheduled") {
		t.Errorf("want 30 requests (100 rps for 300ms):\n%s", stdout)
	}
	count := map[string]int{}
	for _, s := range got() {
		count[s.uri]++
	}
	if count["/a"] != 10 || count["/b"] != 10 || count["/c"] != 10 {
		t.Errorf("counts = %v, want 10 of each: the trace is cycled in order", count)
	}
}

func TestReplayWithAFixedRateAndNoDurationSendsTheTraceOnce(t *testing.T) {
	srv, got := traceServer(t)
	var b strings.Builder
	for _, p := range []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g"} {
		b.WriteString(`{"path":"` + p + `"}` + "\n")
	}
	trace := writeTrace(t, "untimed.jsonl", b.String())

	code, stdout, stderr := execReplay(t, "-file", trace, "-rate", "200", "-arrivals", "even", "-quiet", srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "7 scheduled") {
		t.Errorf("want exactly one pass over the 7 requests:\n%s", stdout)
	}
	if want := "GET /a|GET /b|GET /c|GET /d|GET /e|GET /f|GET /g"; strings.Join(uris(got()), "|") != want {
		t.Errorf("server saw %v, want each request once", uris(got()))
	}
}

const accessLog = `127.0.0.1 - - [10/Oct/2023:13:55:36 +0000] "GET /a HTTP/1.1" 200 5
127.0.0.1 - - [10/Oct/2023:13:55:36 +0000] "GET /b HTTP/1.1" 200 5
this line is not a request
127.0.0.1 - - [10/Oct/2023:13:55:37 +0000] "POST /c HTTP/1.1" 201 5
127.0.0.1 - - [10/Oct/2023:13:55:37 +0000] "GET /d HTTP/1.1" 200 5
127.0.0.1 - - [10/Oct/2023:13:55:38 +0000] "GET /e HTTP/1.1" 200 5 "-" "curl/8.0"
`

func TestReplayAccessLogSkipsWritesUnlessAskedAndSaysSo(t *testing.T) {
	srv, got := traceServer(t)
	logFile := writeTrace(t, "access.log", accessLog)

	// -speed 10 replays the 2 recorded seconds in 0.2s.
	code, stdout, stderr := execReplay(t, "-file", logFile, "-speed", "10", "-arrivals", "even", srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if want := "GET /a|GET /b|GET /d|GET /e"; strings.Join(uris(got()), "|") != want {
		t.Errorf("server saw %v, want only the GETs: a POST in a log is not replayed unless asked", uris(got()))
	}
	if !strings.Contains(stdout, "4 scheduled") {
		t.Errorf("want 4 requests:\n%s", stdout)
	}
	for _, want := range []string{"skipped 1 request", "-methods", "ignored 1 line"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr is missing %q:\n%s", want, stderr)
		}
	}
}

func TestReplayAccessLogSendsWritesWhenAskedTo(t *testing.T) {
	srv, got := traceServer(t)
	logFile := writeTrace(t, "access.log", accessLog)

	code, _, stderr := execReplay(t, "-file", logFile, "-methods", "all", "-speed", "10", "-arrivals", "even", "-quiet", srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	reqs := got()
	if want := "GET /a|GET /b|GET /d|GET /e|POST /c"; strings.Join(uris(reqs), "|") != want {
		t.Errorf("server saw %v, want the POST too", uris(reqs))
	}
	for _, r := range reqs {
		if r.method == "POST" && r.body != "" {
			t.Errorf("a POST from an access log has no body to send, got %q", r.body)
		}
	}
}

func TestReplayUsageErrors(t *testing.T) {
	srv, _ := traceServer(t)
	timed := writeTrace(t, "timed.jsonl", timedTrace)
	untimed := writeTrace(t, "untimed.jsonl", `{"path":"/a"}`+"\n")
	onlyPosts := writeTrace(t, "posts.jsonl", `{"method":"POST","path":"/a"}`+"\n")
	broken := writeTrace(t, "broken.jsonl", `{"path":"/a"}`+"\n"+`{"path":`+"\n")
	oneLeft := writeTrace(t, "one-left.jsonl", `{"at":0,"method":"POST","path":"/a"}`+"\n"+`{"at":0.1,"path":"/b"}`+"\n")
	sameInstant := writeTrace(t, "same-instant.jsonl", `{"at":5,"path":"/a"}`+"\n"+`{"at":5,"path":"/b"}`+"\n")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no file":                          {[]string{srv.URL}, "-file is required"},
		"no url":                           {[]string{"-file", timed}, "missing URL"},
		"two urls":                         {[]string{"-file", timed, srv.URL, srv.URL}, "exactly one URL"},
		"file not found":                   {[]string{"-file", "nosuch.jsonl", srv.URL}, "nosuch.jsonl"},
		"speed with a fixed rate":          {[]string{"-file", untimed, "-rate", "10", "-speed", "2", srv.URL}, "-speed"},
		"duration without a rate":          {[]string{"-file", timed, "-duration", "1s", srv.URL}, "-duration"},
		"untimed trace, no rate":           {[]string{"-file", untimed, srv.URL}, "-rate"},
		"arrivals with exact times":        {[]string{"-file", timed, "-methods", "all", "-arrivals", "poisson", srv.URL}, "-arrivals"},
		"seed with exact times":            {[]string{"-file", timed, "-methods", "all", "-seed", "3", srv.URL}, "-seed"},
		"zero speed":                       {[]string{"-file", timed, "-speed", "0", srv.URL}, "-speed"},
		"negative rate":                    {[]string{"-file", untimed, "-rate", "-5", srv.URL}, "-rate"},
		"bad methods":                      {[]string{"-file", timed, "-methods", "GE T", srv.URL}, "-methods"},
		"empty methods":                    {[]string{"-file", timed, "-methods", "", srv.URL}, "-methods"},
		"bad format":                       {[]string{"-file", timed, "-format", "xml", srv.URL}, "-format"},
		"wrong format for the file":        {[]string{"-file", timed, "-format", "clf", srv.URL}, "common log format"},
		"every request filtered":           {[]string{"-file", onlyPosts, "-rate", "10", srv.URL}, "-methods"},
		"one request left after filtering": {[]string{"-file", oneLeft, srv.URL}, "only 1 request is left"},
		"every request at the same time":   {[]string{"-file", sameInstant, srv.URL}, "same time"},
		"malformed trace":                  {[]string{"-file", broken, "-rate", "10", srv.URL}, "line 2"},
		"not an http url":                  {[]string{"-file", timed, "-methods", "all", "ftp://example.com"}, "url must be"},
		"zero workers":                     {[]string{"-file", timed, "-methods", "all", "-workers", "0", srv.URL}, "workers"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := execReplay(t, tc.args...)
			if code != 2 {
				t.Errorf("exit code %d, want 2 (usage)", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not mention %q", stderr, tc.want)
			}
		})
	}
}

func TestReplayDoesNotLeakCredentialsInTheBaseURL(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)
	out := filepath.Join(t.TempDir(), "run.json")
	withPassword := strings.Replace(srv.URL, "http://", "http://alice:s3cret-pw@", 1)

	code, stdout, stderr := execReplay(t, "-file", trace, "-methods", "all", "-json", out, withPassword)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	raw, _ := os.ReadFile(out)
	for name, text := range map[string]string{"stdout": stdout, "stderr": stderr, "json": string(raw)} {
		if strings.Contains(text, "s3cret-pw") {
			t.Errorf("%s leaks the password:\n%s", name, text)
		}
	}
}

func TestReplayInterruptedStillReportsThePartialRun(t *testing.T) {
	srv, _ := traceServer(t)
	// 3 seconds of recorded traffic, cut off after 250ms.
	trace := writeTrace(t, "long.jsonl", `{"at":0,"path":"/a"}`+"\n"+`{"at":3,"path":"/b"}`+"\n")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	var out, errOut strings.Builder
	code := run(ctx, []string{"replay", "-file", trace, "-quiet", srv.URL}, &out, &errOut)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "interrupted") {
		t.Errorf("stderr should say the run was interrupted:\n%s", errOut.String())
	}
	if !strings.Contains(out.String(), "1 dropped") {
		t.Errorf("the request that never got sent must be reported as dropped:\n%s", out.String())
	}
}

func TestReplayJSONRecordsWhatWasLeftOutAndWhy(t *testing.T) {
	srv, _ := traceServer(t)
	logFile := writeTrace(t, "access.log", accessLog) // 5 usable lines (one a POST) and 1 junk line
	out := filepath.Join(t.TempDir(), "run.json")

	code, _, stderr := execReplay(t, "-file", logFile, "-speed", "10", "-arrivals", "even", "-quiet", "-json", out, srv.URL)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Run struct {
			Replay struct {
				Format   string `json:"format"`
				Requests int    `json:"requests"`
				Skipped  int    `json:"skipped"`
				Unparsed int    `json:"unparsed"`
			} `json:"replay"`
		} `json:"run"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if r := got.Run.Replay; r.Format != "clf" || r.Requests != 4 || r.Skipped != 1 || r.Unparsed != 1 {
		t.Errorf("replay = %+v, want clf, 4 requests replayed, 1 skipped by method, 1 unparsed line", r)
	}
}
