package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimm/Gmeter/internal/report"
)

// CompareInput is what the compare_runs tool takes.
type CompareInput struct {
	Baseline     string  `json:"baseline" jsonschema:"the run_id of the run to compare against, such as before a change"`
	Candidate    string  `json:"candidate" jsonschema:"the run_id of the run to judge, such as after a change"`
	ThresholdPct float64 `json:"threshold_pct,omitempty" jsonschema:"how much higher the candidate's corrected p99 may be, in percent, before it counts as a regression; default 20"`
}

// Row is one figure of both runs. The values are formatted for reading.
type Row struct {
	Metric    string `json:"metric"`
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
	Change    string `json:"change"`
}

// Shift is the share of the candidate's requests slower than the baseline's
// percentile.
type Shift struct {
	Quantile   float64 `json:"quantile"`
	BaselineMs float64 `json:"baseline_ms"`
	Share      float64 `json:"share_slower" jsonschema:"the fraction of the candidate's requests slower than the baseline's value at this quantile; it would be 1 minus the quantile if nothing changed"`
}

// Comparison is what compare_runs returns.
type Comparison struct {
	Summary         string   `json:"summary" jsonschema:"whether the candidate regressed, in a sentence"`
	Regressed       bool     `json:"regressed" jsonschema:"the candidate's corrected p99 is more than threshold_pct above the baseline's"`
	ThresholdPct    float64  `json:"threshold_pct"`
	P99ChangePct    *float64 `json:"p99_change_pct" jsonschema:"the change of corrected p99 in percent; null if the baseline's p99 is zero"`
	ErrorRisePoints *float64 `json:"error_rise_points" jsonschema:"how much the error rate rose, in percentage points; null if either run sent nothing"`
	Notes           []string `json:"notes" jsonschema:"reasons the two runs may not be like-for-like, or that one may not be trustworthy; read these before trusting the comparison"`
	Throughput      []Row    `json:"throughput"`
	Latency         []Row    `json:"latency"`
	Shifts          []Shift  `json:"shifts"`
}

const compareDescription = `Compare two runs made by this server, by run_id, and say whether the second regressed.

The judgement is the change of corrected p99 against threshold_pct (default 20%). Always read notes: they say
when the runs were not like-for-like (a different rate, protocol or arrival pattern) or when one of them was
limited by the generator, and then a regression, or its absence, may not mean what it seems.

Run ids come from load_test and check_slo, and last as long as this server does.`

func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func (s *service) compareRuns(ctx context.Context, req *mcp.CallToolRequest, in CompareInput) (*mcp.CallToolResult, Comparison, error) {
	a, ok := s.runs.get(in.Baseline)
	if !ok {
		return nil, Comparison{}, fmt.Errorf("no run %q: baseline is a run_id returned by load_test or check_slo in this session", in.Baseline)
	}
	b, ok := s.runs.get(in.Candidate)
	if !ok {
		return nil, Comparison{}, fmt.Errorf("no run %q: candidate is a run_id returned by load_test or check_slo in this session", in.Candidate)
	}
	threshold := in.ThresholdPct
	if threshold <= 0 {
		threshold = 20
	}
	c, err := report.Compare(a, b)
	if err != nil {
		return nil, Comparison{}, err
	}

	out := Comparison{
		Regressed: c.Regressed(threshold), ThresholdPct: threshold, P99ChangePct: finite(c.P99ChangePct),
		Notes: append([]string{}, c.Notes...), Throughput: rows(c.Rows), Latency: rows(c.Latency), Shifts: []Shift{},
	}
	if points, ok := c.ErrorRise(); ok {
		out.ErrorRisePoints = finite(points)
	}
	for _, sh := range c.Shifts {
		out.Shifts = append(out.Shifts, Shift{Quantile: sh.Quantile, BaselineMs: float64(sh.Value.Microseconds()) / 1000, Share: sh.Share})
	}

	var sb strings.Builder
	switch {
	case out.P99ChangePct == nil:
		sb.WriteString("The baseline's p99 is zero, so there is no change to measure.")
	case out.Regressed:
		fmt.Fprintf(&sb, "Regressed: corrected p99 rose %.1f%% (%.1fms to %.1fms), past the %.0f%% threshold.", *out.P99ChangePct, a.Corrected.P99Ms, b.Corrected.P99Ms, threshold)
	default:
		fmt.Fprintf(&sb, "No regression: corrected p99 changed %+.1f%% (%.1fms to %.1fms), within the %.0f%% threshold.", *out.P99ChangePct, a.Corrected.P99Ms, b.Corrected.P99Ms, threshold)
	}
	if out.ErrorRisePoints != nil && *out.ErrorRisePoints > 0 {
		fmt.Fprintf(&sb, " The error rate rose %.1f points.", *out.ErrorRisePoints)
	}
	if len(out.Notes) > 0 {
		fmt.Fprintf(&sb, " %d note(s) say the comparison may not be fair: read them.", len(out.Notes))
	}
	out.Summary = sb.String()
	return nil, out, nil
}

func rows(in []report.Row) []Row {
	out := make([]Row, len(in))
	for i, r := range in {
		out[i] = Row{Metric: r.Label, Baseline: r.A, Candidate: r.B, Change: r.Change}
	}
	return out
}

// runURIPrefix is where a run's full report is served as a resource.
const runURIPrefix = "gmeter://runs/"

// readRun serves the full JSON report of a run: the file `gmeter diff` takes.
func (s *service) readRun(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	sum, ok := s.runs.get(strings.TrimPrefix(uri, runURIPrefix))
	if !ok || !strings.HasPrefix(uri, runURIPrefix) {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	raw, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(raw)}}}, nil
}
