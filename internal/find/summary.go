package find

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/report"
)

// SchemaVersion is the version of the JSON layout.
const SchemaVersion = 1

// RunInfo records how the search was run. The URL has any password redacted by
// the caller, and request headers are never recorded.
type RunInfo struct {
	URL            string  `json:"url"`
	Method         string  `json:"method"`
	Workers        int     `json:"workers"`
	TrialSeconds   float64 `json:"trial_seconds"`  // the measured part of each trial
	WarmupSeconds  float64 `json:"warmup_seconds"` // before it, not judged
	TimeoutSeconds float64 `json:"timeout_seconds"`
	SpinSeconds    float64 `json:"spin_seconds"`
	Arrivals       string  `json:"arrivals"`
	Seed           uint64  `json:"seed"` // Poisson only; trial i uses seed+i
	StartedAt      string  `json:"started_at"`
}

// Summary is the serialisable result of a search.
type Summary struct {
	Version int         `json:"version"`
	Run     RunInfo     `json:"run"`
	Target  TargetInfo  `json:"target"`
	Result  ResultInfo  `json:"result"`
	Trials  []TrialInfo `json:"trials"` // always a list, never null

	slo SLO
}

type TargetInfo struct {
	Text         string  `json:"text"` // e.g. "p99 ≤ 50ms"
	Quantile     float64 `json:"quantile"`
	LatencyMs    float64 `json:"latency_ms"`
	MaxErrorRate float64 `json:"max_error_rate"`
}

type ResultInfo struct {
	// Found is whether any rate met the target. MaxRPS is the highest that did
	// and NextFailRPS the lowest rate above it that did not (0 if none was tried).
	Found       bool    `json:"found"`
	MaxRPS      float64 `json:"max_rps"`
	NextFailRPS float64 `json:"next_fail_rps"`
	// LowerBound: the search stopped at its maximum, so MaxRPS is a floor.
	LowerBound bool `json:"lower_bound"`
	// GeneratorLimited: the lowest failing trial could not be trusted because
	// the load generator did not keep time, so the server may sustain more.
	GeneratorLimited bool `json:"generator_limited"`
	Confirmed        int  `json:"confirmed"` // re-runs at MaxRPS that passed...
	ConfirmRuns      int  `json:"confirm_runs"`
	Interrupted      bool `json:"interrupted"`
}

type TrialInfo struct {
	Index      int      `json:"index"` // 1-based
	Phase      string   `json:"phase"`
	RPS        float64  `json:"rps"`
	Verdict    string   `json:"verdict"` // pass, fail or limited
	ObservedMs float64  `json:"observed_ms"`
	ErrorRate  float64  `json:"error_rate"`
	Scheduled  uint64   `json:"scheduled"`
	Reasons    []string `json:"reasons"` // always a list
}

func (v Verdict) String() string {
	switch v {
	case Pass:
		return "pass"
	case Fail:
		return "fail"
	}
	return "limited"
}

// NewSummary digests a search. Each Trial's Detail should be the Observation
// from Evaluate; a trial without one is shown with no measurements.
func NewSummary(o Outcome, info RunInfo, slo SLO, interrupted bool) Summary {
	s := Summary{
		Version: SchemaVersion,
		Run:     info,
		Target: TargetInfo{
			Text:         slo.String(),
			Quantile:     slo.Quantile,
			LatencyMs:    float64(slo.Latency) / float64(time.Millisecond),
			MaxErrorRate: slo.MaxErrorRate,
		},
		Result: ResultInfo{
			Found: o.Best > 0, MaxRPS: o.Best, NextFailRPS: o.NextFail,
			LowerBound: o.LowerBound, GeneratorLimited: o.GeneratorLimited,
			Confirmed: o.Confirmed, ConfirmRuns: o.ConfirmRuns, Interrupted: interrupted,
		},
		Trials: make([]TrialInfo, len(o.Trials)),
		slo:    slo,
	}
	for i, tr := range o.Trials {
		ob, _ := tr.Detail.(Observation)
		reasons := ob.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		s.Trials[i] = TrialInfo{
			Index: i + 1, Phase: string(tr.Phase), RPS: tr.Rate, Verdict: tr.Verdict.String(),
			ObservedMs: float64(ob.Observed) / float64(time.Millisecond), ErrorRate: ob.ErrorRate,
			Scheduled: ob.Scheduled, Reasons: reasons,
		}
	}
	return s
}

// WriteJSON writes the summary as indented JSON.
func (s Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// WriteText writes a human-readable report: what was asked, every trial, and
// the answer with the caveats that decide how far to trust it.
func (s Summary) WriteText(w io.Writer) error {
	var b strings.Builder
	r := s.Result

	warm := ""
	if s.Run.WarmupSeconds > 0 {
		warm = fmt.Sprintf(" after a %gs warm-up", s.Run.WarmupSeconds)
	}
	arrivals := s.Run.Arrivals + " arrivals"
	if s.Run.Arrivals == "poisson" {
		arrivals = fmt.Sprintf("poisson arrivals (seed %d)", s.Run.Seed)
	}
	fmt.Fprintf(&b, "find  %s %s   target %s, errors ≤ %g%%   %s   %gs trials%s\n\n",
		s.Run.Method, s.Run.URL, s.Target.Text, s.Target.MaxErrorRate*100, arrivals, s.Run.TrialSeconds, warm)

	label := s.slo.Label()
	fmt.Fprintf(&b, "  %2s  %-8s  %8s  %9s  %6s  %s\n", "#", "phase", "rate", label, "errors", "result")
	for _, tr := range s.Trials {
		verdict := tr.Verdict
		switch tr.Verdict {
		case "fail":
			verdict = "FAIL"
		}
		if len(tr.Reasons) > 0 {
			verdict += "  " + strings.Join(tr.Reasons, "; ")
		}
		observed := "-"
		if tr.ObservedMs > 0 {
			observed = report.FormatDuration(time.Duration(tr.ObservedMs * float64(time.Millisecond)))
		}
		fmt.Fprintf(&b, "  %2d  %-8s  %8g  %9s  %5.1f%%  %s\n", tr.Index, tr.Phase, tr.RPS, observed, tr.ErrorRate*100, verdict)
	}
	b.WriteString("\n")

	switch {
	case r.Interrupted && r.Found:
		fmt.Fprintf(&b, "result  interrupted: the search did not finish; the best rate found so far is %g rps\n", r.MaxRPS)
	case r.Interrupted:
		b.WriteString("result  interrupted: the search did not finish and no rate had met the target yet\n")
	case !r.Found:
		fmt.Fprintf(&b, "result  no rate down to %g rps met %s\n", r.NextFailRPS, s.Target.Text)
	case r.LowerBound:
		fmt.Fprintf(&b, "result  at least %g rps: the search stopped at its maximum, not because the server gave out (raise -max)\n", r.MaxRPS)
	default:
		fmt.Fprintf(&b, "result  the highest rate meeting %s is about %g rps\n", s.Target.Text, r.MaxRPS)
		if r.NextFailRPS > 0 {
			fmt.Fprintf(&b, "        the next rate up that was tried, %g rps, failed\n", r.NextFailRPS)
		}
	}
	if r.Found && !r.Interrupted && r.ConfirmRuns > 0 {
		if r.Confirmed == r.ConfirmRuns {
			fmt.Fprintf(&b, "        held in %d of %d re-runs at %g rps\n", r.Confirmed, r.ConfirmRuns, r.MaxRPS)
		} else {
			fmt.Fprintf(&b, "        unstable: it held in only %d of %d re-runs at %g rps, so treat it as an upper estimate (a longer -trial gives a steadier answer)\n",
				r.Confirmed, r.ConfirmRuns, r.MaxRPS)
		}
	}
	if r.GeneratorLimited {
		b.WriteString("note    the lowest failing trial was limited by the load generator, not shown to be the server's: the server may sustain more than this, so run Gmeter on a quieter or larger machine\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}
