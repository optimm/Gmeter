package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect starts a server and returns a client talking to it over an in-memory
// pipe: the real protocol, with no process in between.
func connect(t *testing.T, opts Options) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverEnd, clientEnd := mcp.NewInMemoryTransports()
	srv := New(opts)
	go srv.Run(ctx, serverEnd)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientEnd, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call runs a tool and returns its structured result, or the text of the error
// the tool reported.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any, out any) (errText string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: structured result %s: %v", tool, raw, err)
	}
	return ""
}

func target(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func skipIfLimited(t *testing.T, r LoadTestResult) {
	t.Helper()
	if r.GeneratorLimited {
		t.Skipf("the generator could not keep time on this machine: %v", r.Warnings)
	}
}

func TestServerListsItsToolsWithDescriptionsAnAgentCanActOn(t *testing.T) {
	cs := connect(t, Options{})
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tool := range tools.Tools {
		if tool.Name == "load_test" {
			found = true
			for _, want := range []string{"intended", "generator_limited", "loopback"} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("load_test description lacks %q:\n%s", want, tool.Description)
				}
			}
		}
	}
	if !found {
		t.Errorf("no load_test among %d tools", len(tools.Tools))
	}
}

func TestLoadTestMeasuresAServerAndSaysSoInOneLine(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{})

	var got LoadTestResult
	if e := call(t, cs, "load_test", map[string]any{"url": url, "rate": 50, "duration": "1s"}, &got); e != "" {
		t.Fatal(e)
	}
	skipIfLimited(t, got)

	if got.RunID == "" || got.Verdict != "ok" {
		t.Errorf("run %q verdict %q, want an id and ok: %s", got.RunID, got.Verdict, got.Summary)
	}
	if got.Scheduled < 40 || got.Scheduled > 60 || got.OK != got.Responses || got.Corrected.P99Ms <= 0 {
		t.Errorf("got %+v, want about 50 scheduled, all answered, and a p99", got)
	}
	if !strings.Contains(got.Summary, "p99") {
		t.Errorf("summary %q should give the p99", got.Summary)
	}
}

func TestLoadTestCallsTheServerFailingAndSaysWhatWentWrong(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	cs := connect(t, Options{})

	var got LoadTestResult
	if e := call(t, cs, "load_test", map[string]any{"url": url, "rate": 40, "duration": "1s"}, &got); e != "" {
		t.Fatal(e)
	}
	skipIfLimited(t, got)

	if got.Verdict != "errors" || got.Statuses["503"] == 0 || got.BadStatus == 0 {
		t.Errorf("got verdict %q statuses %v, want errors with 503s counted", got.Verdict, got.Statuses)
	}
	if !strings.Contains(got.Summary, "503") {
		t.Errorf("summary %q should name the status", got.Summary)
	}
}

func TestLoadTestSendsTheMethodHeadersAndBodyItWasGiven(t *testing.T) {
	seen := make(chan string, 200)
	url := target(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r.Method + " " + r.Header.Get("X-Test") + " " + r.URL.Path:
		default:
		}
	})
	cs := connect(t, Options{})

	var got LoadTestResult
	args := map[string]any{"url": url + "/items", "method": "DELETE", "headers": map[string]string{"X-Test": "yes"}, "rate": 20, "duration": "500ms"}
	if e := call(t, cs, "load_test", args, &got); e != "" {
		t.Fatal(e)
	}
	if v := <-seen; v != "DELETE yes /items" {
		t.Errorf("server saw %q", v)
	}
}

func TestLoadTestRefusesAHostTheOperatorDidNotAllow(t *testing.T) {
	cs := connect(t, Options{})
	var got LoadTestResult
	e := call(t, cs, "load_test", map[string]any{"url": "http://93.184.216.34", "rate": 10, "duration": "1s"}, &got)
	if !strings.Contains(e, "93.184.216.34") || !strings.Contains(e, "-allow") {
		t.Errorf("error %q, want a refusal that names the host and the -allow flag", e)
	}
}

func TestLoadTestRefusesMoreLoadThanTheOperatorAllowed(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{Policy: Policy{MaxRate: 100, MaxDuration: 5 * time.Second}})
	var got LoadTestResult
	for name, args := range map[string]map[string]any{
		"rate":     {"url": url, "rate": 500, "duration": "1s"},
		"duration": {"url": url, "rate": 10, "duration": "1m"},
		"stages":   {"url": url, "stages": []map[string]any{{"duration": "1s", "from": 10, "to": 900}}},
	} {
		if e := call(t, cs, "load_test", args, &got); !strings.Contains(e, "-max-") {
			t.Errorf("%s: error %q, want a refusal naming the flag that raises the limit", name, e)
		}
	}
}

func TestLoadTestNeedsALoad(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{})
	var got LoadTestResult
	for name, args := range map[string]map[string]any{
		"no load":  {"url": url},
		"no time":  {"url": url, "rate": 10},
		"both":     {"url": url, "rate": 10, "duration": "1s", "stages": []map[string]any{{"duration": "1s", "from": 1, "to": 1}}},
		"bad time": {"url": url, "rate": 10, "duration": "soon"},
	} {
		if e := call(t, cs, "load_test", args, &got); e == "" {
			t.Errorf("%s: want an error saying what to give", name)
		}
	}
}

func TestLoadTestRunsStagesAndReportsEachOne(t *testing.T) {
	url := target(t, func(w http.ResponseWriter, r *http.Request) {})
	cs := connect(t, Options{})
	var got LoadTestResult
	stages := []map[string]any{{"duration": "500ms", "from": 20, "to": 20}, {"duration": "500ms", "from": 60, "to": 60}}
	if e := call(t, cs, "load_test", map[string]any{"url": url, "stages": stages}, &got); e != "" {
		t.Fatal(e)
	}
	skipIfLimited(t, got)
	if len(got.Stages) != 2 || got.Stages[1].Scheduled <= got.Stages[0].Scheduled {
		t.Errorf("stages = %+v, want two, the second busier", got.Stages)
	}
}
