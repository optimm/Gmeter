package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
)

// quickStart has the agents start soon after they get their shares.
func quickStart(t *testing.T) {
	t.Helper()
	old := agentStartIn
	agentStartIn = 50 * time.Millisecond
	t.Cleanup(func() { agentStartIn = old })
}

// agents starts n agents and returns them as -agents takes them.
func agents(t *testing.T, n int) string {
	t.Helper()
	quickStart(t)
	addrs := make([]string, n)
	for i := range addrs {
		addrs[i] = startAgent(t, "-listen", "127.0.0.1:0")
	}
	return strings.Join(addrs, ",")
}

// distributedRun is what a test run on agents saved.
type distributedRun struct {
	Run struct {
		Workers  int    `json:"workers"`
		Profile  string `json:"profile"`
		Arrivals string `json:"arrivals"`
		Seed     uint64 `json:"seed"`
		Agents   int    `json:"agents"`
		Protocol string `json:"protocol"`
		Replay   *struct {
			Requests int `json:"requests"`
		} `json:"replay"`
	} `json:"run"`
	Scheduled uint64            `json:"scheduled"`
	Statuses  map[string]uint64 `json:"statuses"`
	Phases    json.RawMessage   `json:"phases"`
}

// saved runs gmeter with -json and returns what it saved, its stdout and stderr.
func saved(t *testing.T, args ...string) (distributedRun, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.json")
	at := 0
	if len(args) > 0 && args[0] == "replay" {
		at = 1
	}
	args = append(append(append([]string{}, args[:at]...), "-json", path), args[at:]...)
	code, stdout, stderr := exec(t, args...)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r distributedRun
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r, stdout, stderr
}

func TestARunOnAgentsSendsWhatOneMachineWould(t *testing.T) {
	srv, got := traceServer(t)
	run, stdout, stderr := saved(t, "-agents", agents(t, 3), "-rate", "100", "-duration", "300ms", "-workers", "6",
		"-method", "POST", "-H", "X-Test: yes", "-body", "hello", srv.URL+"/x")

	reqs := got()
	if len(reqs) != 30 {
		t.Errorf("the server got %d requests, want 30: three agents at a third of 100 rps for 300ms", len(reqs))
	}
	for _, r := range reqs {
		if r.method != "POST" || r.uri != "/x" || r.body != "hello" || r.header.Get("X-Test") != "yes" {
			t.Errorf("the server got %s %s %q with X-Test %q, want POST /x hello and yes", r.method, r.uri, r.body, r.header.Get("X-Test"))
			break
		}
	}
	if run.Scheduled != 30 || run.Statuses["200"] != 30 || run.Run.Profile != "300ms@100" || run.Run.Workers != 6 || run.Run.Agents != 3 || run.Run.Arrivals != "even" {
		t.Errorf("saved %+v: want the whole test, 30 at 300ms@100 with 6 workers on 3 agents", run)
	}
	if len(run.Phases) == 0 || string(run.Phases) == "null" {
		t.Error("the agents did not split the latency into phases, which -phases asks for by default")
	}
	if !strings.Contains(stdout, "30 ok") {
		t.Errorf("the report does not show the 30 responses:\n%s", stdout)
	}
	if !strings.Contains(stderr, "across 3 agents") || !strings.Contains(stderr, "no live progress") {
		t.Errorf("the banner does not say the test runs on 3 agents, without progress:\n%s", stderr)
	}
}

func TestARunOnAgentsKeepsItsArrivalsAndSeed(t *testing.T) {
	srv := okServer(t)
	run, _, stderr := saved(t, "-agents", agents(t, 2), "-quiet", "-rate", "200", "-duration", "300ms",
		"-arrivals", "poisson", "-seed", "7", srv.URL)
	if run.Run.Arrivals != "poisson" || run.Run.Seed != 7 {
		t.Errorf("saved arrivals %q seed %d, want poisson and 7", run.Run.Arrivals, run.Run.Seed)
	}
	if n := run.Scheduled; n < 30 || n > 100 {
		t.Errorf("%d requests at a Poisson 200 rps for 300ms, want about 60", n)
	}
	if stderr != "" {
		t.Errorf("-quiet, but stderr says:\n%s", stderr)
	}
}

func TestARunOnAgentsReachesAServerOverTLSAndHTTP2AsAskedFor(t *testing.T) {
	https := tlsOKServer(t)
	list := agents(t, 2)
	quick := []string{"-agents", list, "-quiet", "-rate", "100", "-duration", "200ms", "-workers", "2"}

	if _, stdout, _ := saved(t, append(quick, https.URL)...); !strings.Contains(stdout, "tls: 20") {
		t.Errorf("an unverified certificate was trusted:\n%s", stdout)
	}
	if _, stdout, _ := saved(t, append(quick, "-insecure", https.URL)...); !strings.Contains(stdout, "20 ok") {
		t.Errorf("-insecure did not reach the agents:\n%s", stdout)
	}
	if _, stdout, _ := saved(t, append(quick, "-cacert", certFile(t, https), https.URL)...); !strings.Contains(stdout, "20 ok") {
		t.Errorf("-cacert did not reach the agents:\n%s", stdout)
	}
	h2 := h2cServer(t, true)
	if run, stdout, _ := saved(t, append(quick, "-http2", h2.URL)...); !strings.Contains(stdout, "HTTP/2.0: 20") || run.Run.Protocol != "HTTP/2" {
		t.Errorf("-http2 did not reach the agents (protocol %q):\n%s", run.Run.Protocol, stdout)
	}
}

func TestAReplayOnAgentsSendsEveryRequestOnce(t *testing.T) {
	var trace strings.Builder
	for i := range 31 {
		fmt.Fprintf(&trace, `{"at":%g,"path":"/r%d"}`+"\n", float64(i)/100, i)
	}
	file := writeTrace(t, "t.jsonl", trace.String())
	srv, got := traceServer(t)

	run, _, stderr := saved(t, "replay", "-file", file, "-agents", agents(t, 2), srv.URL)
	seen := map[string]int{}
	for _, r := range got() {
		seen[r.uri]++
	}
	for i := range 31 {
		if p := fmt.Sprintf("/r%d", i); seen[p] != 1 {
			t.Errorf("%s was sent %d times, want once", p, seen[p])
		}
	}
	if run.Scheduled != 31 || run.Run.Agents != 2 || run.Run.Replay == nil || run.Run.Replay.Requests != 31 || run.Run.Arrivals != "recorded" {
		t.Errorf("saved %+v, want the 31 recorded requests on 2 agents", run)
	}
	if !strings.Contains(stderr, "across 2 agents") {
		t.Errorf("the banner does not say the replay runs on 2 agents:\n%s", stderr)
	}

	// At a fixed rate the trace is cycled in order, whichever agent sends what.
	srv2, got2 := traceServer(t)
	run, _, _ = saved(t, "replay", "-file", file, "-agents", agents(t, 2), "-quiet", "-arrivals", "even", "-rate", "100", "-duration", "400ms", srv2.URL)
	if n := len(got2()); n != 40 || run.Scheduled != 40 {
		t.Errorf("sent %d, saved %d; want 40, 9 of them a second pass", n, run.Scheduled)
	}
	cycled := map[string]int{}
	for _, r := range got2() {
		cycled[r.uri]++
	}
	for i := range 31 {
		want := 1
		if i < 9 { // the second pass got as far as /r8
			want = 2
		}
		if p := fmt.Sprintf("/r%d", i); cycled[p] != want {
			t.Errorf("%s was sent %d times, want %d", p, cycled[p], want)
		}
	}
}

func TestAgentsAreRefusedBeforeAnythingIsSent(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	one := agents(t, 1)
	closed := closedAddr(t)
	tiny := writeTrace(t, "tiny.jsonl", `{"at":0,"path":"/a"}`+"\n"+`{"at":10,"path":"/b"}`+"\n")

	for _, tc := range []struct {
		name  string
		token string
		args  []string
		code  int
		says  string
	}{
		{"no password", "", []string{"-agents", one, srv.URL}, 2, tokenEnv},
		{"a short password", "pw-1234", []string{"-agents", one, srv.URL}, 2, tokenEnv},
		{"no agent", agentPassword, []string{"-agents", " ", srv.URL}, 2, "-agents"},
		{"an empty entry", agentPassword, []string{"-agents", one + ",,", srv.URL}, 2, "-agents"},
		{"an agent twice", agentPassword, []string{"-agents", one + "," + one, srv.URL}, 2, "twice"},
		{"fewer workers than agents", agentPassword, []string{"-agents", one + "," + closed, "-workers", "1", srv.URL}, 2, "-workers"},
		{"a bad configuration", agentPassword, []string{"-agents", one, "-timeout", "-1s", srv.URL}, 2, "timeout"},
		{"fewer recorded requests than agents", agentPassword, []string{"replay", "-file", tiny, "-agents", one + "," + closed + ",127.0.0.1:1", srv.URL}, 2, "3 agents"},
		{"an agent away", agentPassword, []string{"-agents", one + "," + closed, srv.URL}, 1, closed},
		{"the wrong password", agentPassword + "-not", []string{"-agents", one, srv.URL}, 1, "401"},
		{"a capacity search", agentPassword, []string{"find", "-agents", one, srv.URL}, 2, "agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tokenEnv, tc.token)
			code, stdout, stderr := exec(t, tc.args...)
			if code != tc.code || !strings.Contains(stderr, tc.says) {
				t.Errorf("exit %d, stderr:\n%s\nwant %d and %q", code, stderr, tc.code, tc.says)
			}
			if stdout != "" {
				t.Errorf("a report of a test that did not run:\n%s", stdout)
			}
			if tc.token != "" && strings.Contains(stderr+stdout, tc.token) {
				t.Errorf("the password is in the output:\n%s", stderr)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("%d requests were sent", n)
			}
		})
	}
}

// closedAddr is an address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestInterruptingARunOnAgentsStopsThemWithoutAReport(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	list := agents(t, 2)

	ctx, cancel := context.WithCancel(context.Background())
	var stdout bytes.Buffer
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-agents", list, "-rate", "200", "-duration", "1m", srv.URL}, &stdout, &stderr)
	}()
	for deadline := time.Now().Add(5 * time.Second); hits.Load() < 20 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), "no partial report") {
			t.Errorf("exit %d, stderr:\n%s\nwant 1 and that there is no partial report", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not stop")
	}
	if stdout.Len() != 0 {
		t.Errorf("a report of part of a test:\n%s", stdout.String())
	}
	// The controller returns as soon as it has dropped its connections; an
	// agent stops when it notices, which takes it a moment.
	time.Sleep(200 * time.Millisecond)
	after := hits.Load()
	time.Sleep(300 * time.Millisecond)
	if more := hits.Load() - after; more > 0 {
		t.Errorf("the agents were still sending %d requests 200ms after the run was stopped", more)
	}
}

func TestTheJobSentToAgentsCarriesWhatTheFlagsSay(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	rf := addRequestFlags(fs, "even", 50)
	addPhasesFlag(fs, rf)
	if err := fs.Parse([]string{"-workers", "7", "-timeout", "3s", "-spin", "1ms", "-arrivals", "poisson", "-seed", "9", "-cacert", certFile(t, tlsOKServer(t))}); err != nil {
		t.Fatal(err)
	}
	cfg, err := rf.base("https://example.test/")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Stages, cfg.Arrivals, cfg.Seed = pacer.ConstantRate(10, time.Second), pacer.Poisson, 9
	j, err := rf.job(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if j.Arrivals != "poisson" || j.Seed != 9 || j.Workers != 7 || j.Timeout != 3*time.Second || j.Spin != time.Millisecond ||
		j.SampleEvery != sampleEvery || j.StartIn != agentStartIn || len(j.CACert) == 0 {
		t.Errorf("job = %+v, want what the flags and the config say", j)
	}
}

func TestAnAgentTimesRequestsOutAsAsked(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	_, stdout, _ := saved(t, "-agents", agents(t, 2), "-quiet", "-rate", "50", "-duration", "200ms", "-timeout", "50ms", slow.URL)
	if !strings.Contains(stdout, "timeout: 10") {
		t.Errorf("the agents did not time the 10 requests out after 50ms:\n%s", stdout)
	}
}
