package mcpserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimm/Gmeter"
	"github.com/optimm/Gmeter/internal/report"
)

// StageInput is one step or ramp of a load profile.
type StageInput struct {
	Duration string  `json:"duration" jsonschema:"how long the stage lasts, such as 30s or 2m"`
	From     float64 `json:"from" jsonschema:"requests per second at the start of the stage"`
	To       float64 `json:"to" jsonschema:"requests per second at the end: the same as from holds the rate, higher ramps up, 0 for both pauses"`
}

// GRPCInput says which unary gRPC method to call.
type GRPCInput struct {
	Method    string              `json:"method" jsonschema:"the method to call, as package.Service/Method; its schema is read from the server by reflection"`
	Data      string              `json:"data,omitempty" jsonschema:"the request message as JSON in the protobuf mapping; empty sends the empty message"`
	Plaintext bool                `json:"plaintext,omitempty" jsonschema:"speak gRPC without TLS, for a server that does not use it"`
	Metadata  map[string][]string `json:"metadata,omitempty" jsonschema:"metadata sent with every call, gRPC's counterpart of headers"`
}

// TargetInput says what to send load to: the part of a request every tool shares.
type TargetInput struct {
	URL      string            `json:"url" jsonschema:"what to load: http://host:port/path or https://..., or host:port for gRPC"`
	Method   string            `json:"method,omitempty" jsonschema:"HTTP method, default GET; not for gRPC"`
	Headers  map[string]string `json:"headers,omitempty" jsonschema:"HTTP headers sent with every request; not for gRPC"`
	Body     string            `json:"body,omitempty" jsonschema:"HTTP request body sent with every request; not for gRPC"`
	GRPC     *GRPCInput        `json:"grpc,omitempty" jsonschema:"call a unary gRPC method instead of sending HTTP requests"`
	Workers  int               `json:"workers,omitempty" jsonschema:"most requests in flight, which is also the most connections; default 50"`
	Timeout  string            `json:"timeout,omitempty" jsonschema:"per request, such as 5s; default 10s"`
	HTTP2    bool              `json:"http2,omitempty" jsonschema:"send over HTTP/2 only"`
	Insecure bool              `json:"insecure_skip_verify,omitempty" jsonschema:"do not verify the server's TLS certificate, for a self-signed one"`
}

// LoadTestInput is what the load_test tool takes.
type LoadTestInput struct {
	TargetInput
	Rate     float64      `json:"rate,omitempty" jsonschema:"requests per second, held for duration; give this and duration, or stages"`
	Duration string       `json:"duration,omitempty" jsonschema:"how long to send at rate, such as 10s"`
	Stages   []StageInput `json:"stages,omitempty" jsonschema:"a load profile of steps and ramps, instead of rate and duration"`
	Poisson  bool         `json:"poisson,omitempty" jsonschema:"spread requests at random with the same average rate, as real traffic arrives in bursts"`
	Seed     uint64       `json:"seed,omitempty" jsonschema:"with poisson, repeat the same arrival times"`
}

// LatencyMS is a latency distribution in milliseconds.
type LatencyMS struct {
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P90Ms  float64 `json:"p90_ms"`
	P99Ms  float64 `json:"p99_ms"`
	P999Ms float64 `json:"p999_ms"`
	MaxMs  float64 `json:"max_ms"`
}

// StageOutput is how one stage of the profile went.
type StageOutput struct {
	Profile   string    `json:"profile"`
	TargetRPS float64   `json:"target_rps"`
	Scheduled uint64    `json:"scheduled"`
	Failed    uint64    `json:"failed"`
	Corrected LatencyMS `json:"corrected"`
}

// LoadTestResult is what load_test returns.
type LoadTestResult struct {
	Summary          string            `json:"summary" jsonschema:"the result in one or two sentences"`
	Verdict          string            `json:"verdict" jsonschema:"ok; errors if requests failed or were left unsent; generator_limited if the load generator could not keep time, in which case the latencies are not the server's"`
	RunID            string            `json:"run_id" jsonschema:"refers to this run in later calls"`
	TargetRPS        float64           `json:"target_rps"`
	AchievedRPS      float64           `json:"achieved_rps"`
	Scheduled        uint64            `json:"scheduled"`
	Dropped          uint64            `json:"dropped" jsonschema:"requests that were due but never sent: the generator could not keep up"`
	Responses        uint64            `json:"responses"`
	OK               uint64            `json:"ok"`
	BadStatus        uint64            `json:"bad_status" jsonschema:"responses with a status of 400 or more"`
	Failed           uint64            `json:"failed" jsonschema:"requests that got no response: timeout, refused, reset"`
	Statuses         map[string]uint64 `json:"statuses"`
	Codes            map[string]uint64 `json:"codes,omitempty" jsonschema:"gRPC status codes of the answers"`
	Errors           map[string]uint64 `json:"errors"`
	Corrected        LatencyMS         `json:"corrected" jsonschema:"latency from when each request was due: what users experienced"`
	Service          LatencyMS         `json:"service" jsonschema:"latency from when each request was sent: what a closed-loop tester would report"`
	GeneratorLimited bool              `json:"generator_limited"`
	TimerLagP99Ms    float64           `json:"timer_lag_p99_ms" jsonschema:"how late the generator's timers fired: over 5 means it was starved"`
	Warnings         []string          `json:"warnings"`
	Stages           []StageOutput     `json:"stages,omitempty" jsonschema:"one entry per stage, when there was more than one"`
}

const loadTestDescription = `Send a steady or shaped load at an HTTP or unary gRPC service and report its latency.

Requests are sent on a clock, at the rate asked for, whatever the server does; latency is measured from the
intended send time ("corrected"), so queueing behind a slow server counts. Give either rate and duration,
or stages for steps and ramps.

The verdict is ok, errors, or generator_limited. generator_limited means the load generator itself could not
keep time (timer lag), so the numbers describe the generator and not the server: lower the rate or the workers
and run again before concluding anything.

Only loopback targets are allowed unless the operator started the server with -allow; rate, duration and
workers have ceilings that a refusal names. Do not try to get around a refusal: tell the operator.`

func (s *service) addTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "load_test",
		Title:       "Load test an HTTP or gRPC service",
		Description: loadTestDescription,
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptr(true), DestructiveHint: ptr(false)},
	}, s.loadTest)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "find_capacity",
		Title:       "Find the highest rate a service holds",
		Description: capacityDescription,
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptr(true), DestructiveHint: ptr(false)},
	}, s.findCapacity)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "check_slo",
		Title:       "Check a service against a latency target",
		Description: checkDescription,
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptr(true), DestructiveHint: ptr(false)},
	}, s.checkSLO)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "compare_runs",
		Title:       "Compare two runs",
		Description: compareDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.compareRuns)
	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: runURIPrefix + "{run_id}",
		Name:        "run",
		Title:       "A run's full report",
		Description: "The full JSON report of a run made by this server, including its latency histograms, as gmeter diff reads it.",
		MIMEType:    "application/json",
	}, s.readRun)
}

func ptr[T any](v T) *T { return &v }

// target turns the target settings into a gmeter.Config with no load, refusing a
// target the policy does not allow.
func (s *service) target(ctx context.Context, in TargetInput) (cfg gmeter.Config, err error) {
	if in.URL == "" {
		return cfg, fmt.Errorf("url is required")
	}
	if err := s.policy.CheckTarget(ctx, in.URL); err != nil {
		return cfg, err
	}
	cfg = gmeter.Config{URL: in.URL, Method: in.Method, Workers: in.Workers, HTTP2: in.HTTP2}
	if in.Body != "" { // an empty body is not "no body" to a gRPC call, which refuses one
		cfg.Body = []byte(in.Body)
	}
	if len(in.Headers) > 0 {
		cfg.Header = http.Header{}
		for k, v := range in.Headers {
			cfg.Header.Set(k, v)
		}
	}
	if in.Timeout != "" {
		if cfg.Timeout, err = time.ParseDuration(in.Timeout); err != nil {
			return cfg, fmt.Errorf("timeout %q is not a duration such as 5s", in.Timeout)
		}
	}
	if in.Insecure {
		cfg.TLS = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // asked for, for a self-signed server
	}
	if g := in.GRPC; g != nil {
		cfg.GRPC = &gmeter.GRPC{Method: g.Method, Plaintext: g.Plaintext, Metadata: g.Metadata}
		if g.Data != "" {
			cfg.GRPC.Data = []byte(g.Data)
		}
	}
	return cfg, nil
}

// effectiveWorkers is how many workers a config runs with.
func effectiveWorkers(cfg gmeter.Config) int {
	if cfg.Workers == 0 {
		return 50
	}
	return cfg.Workers
}

// config turns the tool's input into a gmeter.Config, refusing what the policy
// does not allow. It returns the total duration of the load, which is what the
// progress is measured against.
func (s *service) config(ctx context.Context, in LoadTestInput) (cfg gmeter.Config, total time.Duration, err error) {
	if cfg, err = s.target(ctx, in.TargetInput); err != nil {
		return cfg, 0, err
	}
	cfg.Poisson, cfg.Seed = in.Poisson, in.Seed

	var peak float64
	switch {
	case len(in.Stages) > 0 && (in.Rate != 0 || in.Duration != ""):
		return cfg, 0, fmt.Errorf("give either rate and duration, or stages, not both")
	case len(in.Stages) > 0:
		for i, st := range in.Stages {
			d, err := time.ParseDuration(st.Duration)
			if err != nil || d <= 0 {
				return cfg, 0, fmt.Errorf("stage %d: duration %q is not a positive duration such as 30s", i+1, st.Duration)
			}
			cfg.Stages = append(cfg.Stages, gmeter.Stage{Duration: d, From: st.From, To: st.To})
			total += d
			peak = max(peak, st.From, st.To)
		}
	case in.Rate > 0 && in.Duration != "":
		if cfg.Duration, err = time.ParseDuration(in.Duration); err != nil || cfg.Duration <= 0 {
			return cfg, 0, fmt.Errorf("duration %q is not a positive duration such as 10s", in.Duration)
		}
		cfg.Rate, peak, total = in.Rate, in.Rate, cfg.Duration
	default:
		return cfg, 0, fmt.Errorf("give a load: rate and duration (such as rate 100, duration 10s), or stages")
	}
	return cfg, total, s.policy.CheckLoad(peak, total, effectiveWorkers(cfg))
}

func (s *service) loadTest(ctx context.Context, req *mcp.CallToolRequest, in LoadTestInput) (*mcp.CallToolResult, LoadTestResult, error) {
	out, _, err := s.runLoad(ctx, req, in)
	return nil, out, err
}

// runLoad runs the load and records the run. It returns the gmeter.Result as
// well, for a tool that judges it.
func (s *service) runLoad(ctx context.Context, req *mcp.CallToolRequest, in LoadTestInput) (LoadTestResult, *gmeter.Result, error) {
	cfg, total, err := s.config(ctx, in)
	if err != nil {
		return LoadTestResult{}, nil, err
	}
	stop := reportProgress(ctx, req, "load test", total)
	started := time.Now()
	res, err := gmeter.Run(ctx, cfg)
	stop()
	if err != nil {
		return LoadTestResult{}, nil, err
	}
	sum, err := summarize(res)
	if err != nil {
		return LoadTestResult{}, nil, err
	}
	sum.Run = runInfo(cfg, started)
	out := resultOf(sum)
	out.RunID = s.runs.add(sum)
	return out, res, nil
}

// summarize reads the report a Result writes, which is the one record of a run
// that carries the generator's health and the per-stage figures.
func summarize(res *gmeter.Result) (report.Summary, error) {
	var buf bytes.Buffer
	if err := res.WriteJSON(&buf); err != nil {
		return report.Summary{}, err
	}
	var sum report.Summary
	if err := json.Unmarshal(buf.Bytes(), &sum); err != nil {
		return report.Summary{}, err
	}
	return sum, nil
}

// runInfo says what was run, so that two runs can be checked for being alike. The
// URL loses any password, and headers are never recorded.
func runInfo(cfg gmeter.Config, started time.Time) report.RunInfo {
	info := report.RunInfo{
		URL: redact(cfg.URL), Method: cfg.Method, Workers: cfg.Workers, Arrivals: "even",
		Seed: cfg.Seed, StartedAt: started.UTC().Format(time.RFC3339),
	}
	if cfg.Poisson {
		info.Arrivals = "poisson"
	}
	if cfg.GRPC != nil {
		info.Method = "gRPC " + cfg.GRPC.Method
	} else if info.Method == "" {
		info.Method = http.MethodGet
	}
	if cfg.Workers == 0 {
		info.Workers = 50
	}
	if cfg.HTTP2 {
		info.Protocol = "HTTP/2"
	}
	return info
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func latency(l report.Latency) LatencyMS {
	return LatencyMS{MeanMs: l.MeanMs, P50Ms: l.P50Ms, P90Ms: l.P90Ms, P99Ms: l.P99Ms, P999Ms: l.P999Ms, MaxMs: l.MaxMs}
}

// resultOf turns a run's summary into what the tool returns.
func resultOf(sum report.Summary) LoadTestResult {
	out := LoadTestResult{
		TargetRPS: sum.TargetRPS, AchievedRPS: sum.AchievedRPS,
		Scheduled: sum.Scheduled, Dropped: sum.Dropped, Responses: sum.Responses,
		OK: sum.OK, BadStatus: sum.BadStatus, Failed: sum.Failed,
		Statuses: map[string]uint64{}, Errors: map[string]uint64{},
		Corrected: latency(sum.Corrected), Service: latency(sum.Service),
		TimerLagP99Ms: sum.Generator.TimerLagP99Ms,
		Warnings:      append([]string{}, sum.Warnings...),
	}
	for code, n := range sum.Statuses {
		out.Statuses[fmt.Sprint(code)] = n
	}
	for k, n := range sum.Errors {
		out.Errors[k] = n
	}
	if len(sum.Codes) > 0 {
		out.Codes = map[string]uint64{}
		for k, n := range sum.Codes {
			out.Codes[k] = n
		}
	}
	out.GeneratorLimited = sum.Generator.TimerLagP99Ms > float64(report.TimerLagWarn)/float64(time.Millisecond)
	switch {
	case out.GeneratorLimited:
		out.Verdict = "generator_limited"
	case out.Failed+out.BadStatus+out.Dropped > 0:
		out.Verdict = "errors"
	default:
		out.Verdict = "ok"
	}
	if len(sum.Stages) > 1 {
		for _, st := range sum.Stages {
			out.Stages = append(out.Stages, StageOutput{
				Profile: st.Profile, TargetRPS: st.TargetRPS, Scheduled: st.Scheduled, Failed: st.Failed,
				Corrected: latency(st.Corrected),
			})
		}
	}
	out.Summary = sentence(out)
	return out
}

// sentence says the result in a line, with the figure that matters most for its
// verdict first.
func sentence(r LoadTestResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sent %d requests at %.0f/s target (%.0f/s achieved): %d ok", r.Scheduled, r.TargetRPS, r.AchievedRPS, r.OK)
	if r.BadStatus > 0 {
		bad := counts(r.Statuses, func(k string) bool { return k >= "400" })
		if codes := counts(r.Codes, func(k string) bool { return k != "OK" }); codes != "" {
			bad = codes
		}
		fmt.Fprintf(&b, ", %d bad status (%s)", r.BadStatus, bad)
	}
	if r.Failed > 0 {
		fmt.Fprintf(&b, ", %d failed (%s)", r.Failed, counts(r.Errors, nil))
	}
	if r.Dropped > 0 {
		fmt.Fprintf(&b, ", %d never sent", r.Dropped)
	}
	fmt.Fprintf(&b, ". Corrected p99 %.1fms (p50 %.1fms, max %.1fms); service p99 %.1fms.", r.Corrected.P99Ms, r.Corrected.P50Ms, r.Corrected.MaxMs, r.Service.P99Ms)
	if r.GeneratorLimited {
		fmt.Fprintf(&b, " The generator could not keep time (timer lag p99 %.1fms): these latencies are not the server's.", r.TimerLagP99Ms)
	}
	return b.String()
}

// counts writes "503 x40, 500 x2", the largest first, for the keys keep accepts.
func counts(m map[string]uint64, keep func(string) bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if keep == nil || keep(k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s x%d", k, m[k])
	}
	return strings.Join(parts, ", ")
}
