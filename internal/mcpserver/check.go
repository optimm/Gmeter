package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimm/Gmeter"
)

// SLOInput is what the check_slo tool takes: a load, and the target to judge it by.
type SLOInput struct {
	LoadTestInput
	SLO          string  `json:"slo" jsonschema:"the target to meet, as quantile=latency: p99=50ms, or p99.9=200ms"`
	MaxErrorRate float64 `json:"max_error_rate,omitempty" jsonschema:"the share of requests that may fail or get a bad status, as a fraction: 0.01 allows 1%; default none"`
}

// SLOResult is what check_slo returns.
type SLOResult struct {
	Summary string         `json:"summary" jsonschema:"pass or miss, and why, in a sentence"`
	Pass    bool           `json:"pass"`
	Reasons []string       `json:"reasons" jsonschema:"why the target was missed; empty on a pass"`
	Run     LoadTestResult `json:"run" jsonschema:"the run it was judged on"`
}

const checkDescription = `Run a load and say whether the service met a latency target, with the reasons if not.

Use it as a yes-or-no gate: "does the service hold 200/s within p99=50ms?". It misses the target when the
latency at the target's quantile is over the limit, when more requests failed than max_error_rate allows,
when requests were left unsent, or when the load generator could not keep time: in that case a good-looking
result would be the generator's, not the service's, and it says so rather than passing.

Takes the same load and target as load_test, plus slo. The full run is returned, and is kept under its run_id.`

func (s *service) checkSLO(ctx context.Context, req *mcp.CallToolRequest, in SLOInput) (*mcp.CallToolResult, SLOResult, error) {
	slo, err := parseSLO(in.SLO, in.MaxErrorRate)
	if err != nil {
		return nil, SLOResult{}, err
	}
	run, res, err := s.runLoad(ctx, req, in.LoadTestInput)
	if err != nil {
		return nil, SLOResult{}, err
	}
	out := SLOResult{Run: run, Reasons: []string{}}
	switch err := res.Meets(slo); {
	case err == nil:
		out.Pass = true
		out.Summary = fmt.Sprintf("Met %s: corrected p99 %.1fms over %d requests.", in.SLO, run.Corrected.P99Ms, run.Scheduled)
	case errors.Is(err, gmeter.ErrTargetMissed):
		out.Reasons = strings.Split(strings.TrimPrefix(err.Error(), gmeter.ErrTargetMissed.Error()+": "), "; ")
		out.Summary = fmt.Sprintf("Missed %s: %s.", in.SLO, strings.Join(out.Reasons, "; "))
	default:
		return nil, SLOResult{}, err
	}
	return nil, out, nil
}
