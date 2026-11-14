package mcpserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	"github.com/optimm/Gmeter/internal/grpcdemo"
)

func slow(d time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { time.Sleep(d) }
}

func TestFindCapacityReportsTheHighestRateThatMeetsTheTarget(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{})

	var got CapacityResult
	args := map[string]any{"url": url, "slo": "p99=1s", "start": 20, "max": 40, "trial": "300ms", "warmup": "0s", "confirm": 0}
	if e := call(t, cs, "find_capacity", args, &got); e != "" {
		t.Fatal(e)
	}
	if got.GeneratorLimited {
		t.Skipf("the generator could not keep time: %s", got.Summary)
	}

	if !got.Found || got.MaxRPS != 40 || !got.LowerBound || len(got.Trials) < 2 {
		t.Errorf("got %+v, want 40/s found as a lower bound after at least two trials", got)
	}
	if !strings.Contains(got.Summary, "at least 40") {
		t.Errorf("summary %q should say the server takes at least 40/s", got.Summary)
	}
}

func TestFindCapacityOfAServerThatNeverMeetsTheTargetSaysWhy(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	cs := connect(t, Options{})

	var got CapacityResult
	args := map[string]any{"url": url, "slo": "p99=1s", "start": 8, "max": 16, "trial": "200ms", "warmup": "0s", "confirm": 0}
	if e := call(t, cs, "find_capacity", args, &got); e != "" {
		t.Fatal(e)
	}
	if got.Found || len(got.Trials) == 0 || len(got.Trials[0].Reasons) == 0 || !strings.Contains(got.Summary, "No rate") {
		t.Errorf("got %+v, want nothing found, with the reason the first trial failed", got)
	}
}

func TestFindCapacityNeverSearchesAboveTheOperatorsRateLimit(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{Policy: Policy{MaxRate: 40}})

	var got CapacityResult
	args := map[string]any{"url": url, "slo": "p99=1s", "start": 20, "trial": "200ms", "warmup": "0s", "confirm": 0}
	if e := call(t, cs, "find_capacity", args, &got); e != "" {
		t.Fatal(e)
	}
	for _, tr := range got.Trials {
		if tr.RPS > 40 {
			t.Errorf("tried %v/s, over the limit of 40/s", tr.RPS)
		}
	}

	args["max"] = 500
	if e := call(t, cs, "find_capacity", args, &got); !strings.Contains(e, "-max-rate") {
		t.Errorf("error %q, want a refusal naming -max-rate", e)
	}
	args["max"], args["trial"] = 30, "10m"
	if e := call(t, cs, "find_capacity", args, &got); !strings.Contains(e, "-max-duration") {
		t.Errorf("error %q, want a refusal naming -max-duration", e)
	}
}

func TestFindCapacityNeedsATargetItCanRead(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{})
	var got CapacityResult
	for name, args := range map[string]map[string]any{
		"none":    {"url": url},
		"garbled": {"url": url, "slo": "fast please"},
	} {
		if e := call(t, cs, "find_capacity", args, &got); e == "" {
			t.Errorf("%s: want an error saying how to write the target", name)
		}
	}
}

func TestCheckSLOPassesAServerThatMeetsTheTarget(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{})

	var got SLOResult
	if e := call(t, cs, "check_slo", map[string]any{"url": url, "rate": 40, "duration": "1s", "slo": "p99=1s"}, &got); e != "" {
		t.Fatal(e)
	}
	if got.Run.GeneratorLimited {
		t.Skip("the generator could not keep time")
	}
	if !got.Pass || len(got.Reasons) != 0 || got.Run.RunID == "" {
		t.Errorf("got %+v, want a pass with no reasons and the run it was judged on", got)
	}
}

func TestCheckSLOSaysWhyATargetWasMissed(t *testing.T) {
	url := target(t, slow(30*time.Millisecond))
	cs := connect(t, Options{})

	var got SLOResult
	if e := call(t, cs, "check_slo", map[string]any{"url": url, "rate": 20, "duration": "1s", "slo": "p99=5ms"}, &got); e != "" {
		t.Fatal(e)
	}
	if got.Pass || len(got.Reasons) == 0 || !strings.Contains(strings.Join(got.Reasons, " "), "p99") {
		t.Errorf("got %+v, want a miss that names the p99", got)
	}
}

func TestCheckSLOAllowsTheErrorsItWasGiven(t *testing.T) {
	var n int
	var mu sync.Mutex
	url := target(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		bad := n%10 == 0
		mu.Unlock()
		if bad {
			w.WriteHeader(500)
		}
	})
	cs := connect(t, Options{})
	args := map[string]any{"url": url, "rate": 50, "duration": "1s", "slo": "p99=1s"}

	var strict, lenient SLOResult
	call(t, cs, "check_slo", args, &strict)
	args["max_error_rate"] = 0.2
	call(t, cs, "check_slo", args, &lenient)
	if strict.Run.GeneratorLimited {
		t.Skip("the generator could not keep time")
	}
	if strict.Pass || !lenient.Pass {
		t.Errorf("strict pass = %v (%v), lenient pass = %v (%v); want the 10%% errors to fail only the strict one",
			strict.Pass, strict.Reasons, lenient.Pass, lenient.Reasons)
	}
}

func TestCompareRunsFlagsARegressionBetweenTwoRuns(t *testing.T) {
	fast := target(t, func(w http.ResponseWriter, r *http.Request) {})
	slower := target(t, slow(25*time.Millisecond))
	cs := connect(t, Options{})

	var a, b LoadTestResult
	call(t, cs, "load_test", map[string]any{"url": fast, "rate": 20, "duration": "1s"}, &a)
	call(t, cs, "load_test", map[string]any{"url": slower, "rate": 20, "duration": "1s"}, &b)
	if a.GeneratorLimited || b.GeneratorLimited {
		t.Skip("the generator could not keep time")
	}

	var got Comparison
	if e := call(t, cs, "compare_runs", map[string]any{"baseline": a.RunID, "candidate": b.RunID}, &got); e != "" {
		t.Fatal(e)
	}
	if !got.Regressed || got.P99ChangePct == nil || *got.P99ChangePct <= 20 || len(got.Latency) == 0 {
		t.Errorf("got %+v, want a regression of more than 20%% with the latency rows", got)
	}
	if !strings.Contains(got.Summary, "p99") {
		t.Errorf("summary %q should give the p99 change", got.Summary)
	}

	var same Comparison
	call(t, cs, "compare_runs", map[string]any{"baseline": a.RunID, "candidate": a.RunID}, &same)
	if same.Regressed {
		t.Errorf("a run compared with itself regressed: %+v", same)
	}
}

func TestCompareRunsNamesARunItDoesNotKnow(t *testing.T) {
	cs := connect(t, Options{})
	var got Comparison
	e := call(t, cs, "compare_runs", map[string]any{"baseline": "run-nope", "candidate": "run-nada"}, &got)
	if !strings.Contains(e, "run-nope") {
		t.Errorf("error %q, want it to name the run that is unknown", e)
	}
}

func TestARunCanBeReadBackAsTheFullReport(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{})
	var run LoadTestResult
	call(t, cs, "load_test", map[string]any{"url": url, "rate": 20, "duration": "500ms"}, &run)

	res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "gmeter://runs/" + run.RunID})
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if len(res.Contents) != 1 || json.Unmarshal([]byte(res.Contents[0].Text), &report) != nil || report["latency_corrected"] == nil {
		t.Errorf("contents %+v, want the run's JSON report", res.Contents)
	}

	if _, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "gmeter://runs/run-nope"}); err == nil {
		t.Error("reading a run that does not exist should fail")
	}
}

func TestALongRunReportsProgressWhileItRuns(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	var mu sync.Mutex
	var seen []*mcp.ProgressNotificationParams

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverEnd, clientEnd := mcp.NewInMemoryTransports()
	go New(Options{}).Run(ctx, serverEnd)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			seen = append(seen, r.Params)
			mu.Unlock()
		},
	})
	cs, err := client.Connect(ctx, clientEnd, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	params := &mcp.CallToolParams{Name: "load_test", Arguments: map[string]any{"url": url, "rate": 20, "duration": "2s"}}
	params.SetProgressToken("p1")
	if _, err := cs.CallTool(ctx, params); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond) // a notification may arrive just after the result
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("got %d progress notifications, want at least 2 in a 2s run", len(seen))
	}
	last := seen[len(seen)-1]
	if last.Total != 2 || last.Progress <= 0 || last.Progress > 2 || last.Message == "" {
		t.Errorf("last = %+v, want progress within a total of 2 seconds, with a message", last)
	}
}

func TestLoadTestCallsAUnaryGRPCMethod(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	grpcdemo.Register(s, grpcdemo.ReflectionBoth)
	go s.Serve(ln)
	t.Cleanup(s.Stop)
	cs := connect(t, Options{})

	var got LoadTestResult
	args := map[string]any{
		"url": ln.Addr().String(), "rate": 50, "duration": "1s",
		"grpc": map[string]any{"method": grpcdemo.Say, "data": `{"name":"ann"}`, "plaintext": true},
	}
	if e := call(t, cs, "load_test", args, &got); e != "" {
		t.Fatal(e)
	}
	skipIfLimited(t, got)
	if got.Verdict != "ok" || got.Codes["OK"] < 40 || got.OK != got.Responses {
		t.Errorf("got verdict %q codes %v, want ok with the gRPC OK count", got.Verdict, got.Codes)
	}

	args["grpc"] = map[string]any{"method": grpcdemo.Say, "data": `{"code":"NOT_FOUND"}`, "plaintext": true}
	call(t, cs, "load_test", args, &got)
	if got.Verdict != "errors" || got.Codes["NOT_FOUND"] == 0 || !strings.Contains(got.Summary, "NOT_FOUND") {
		t.Errorf("got verdict %q codes %v summary %q, want errors that name NOT_FOUND", got.Verdict, got.Codes, got.Summary)
	}
}
