package report

import (
	"fmt"
	"math"
	"strings"

	"github.com/optimm/Gmeter/internal/runner"
)

// PhaseMoveFraction is how much of A's mean latency a phase's time per request
// has to change by for MainPhase to name it. Two runs of the same thing differ
// a little in every phase, and naming the one that happened to differ most
// would present noise as a finding.
const PhaseMoveFraction = 0.10

// PhaseRow is one phase in both runs.
type PhaseRow struct {
	Phase string

	// PerRequestA and PerRequestB are the time an answered request spent in the
	// phase, on average, in milliseconds. A phase only some requests have (a
	// connect) counts as zero for the others, and each run is averaged over its
	// own answered requests, so over all phases they add up to each run's mean
	// latency and the runs can be compared whatever they answered.
	PerRequestA, PerRequestB float64

	// P99A and P99B are the phase's p99 over the requests that had it, in
	// milliseconds; SamplesA and SamplesB say how many that is. P99ChangePct is
	// NaN if either run has no samples, or A's p99 is zero.
	P99A, P99B         float64
	SamplesA, SamplesB uint64
	P99ChangePct       float64

	// ShareA and ShareB are the phase's part of the run's latency, in percent.
	ShareA, ShareB float64
}

// ChangeMs is how much more time an answered request spent in the phase in B
// than in A, in milliseconds: negative if it spent less.
func (r PhaseRow) ChangeMs() float64 { return r.PerRequestB - r.PerRequestA }

// comparePhases matches the phases of two runs, in the order a request goes
// through them. A phase only one run has is a row too: that run's time in it is
// the whole change.
func comparePhases(a, b []PhaseSummary) []PhaseRow {
	index := func(phases []PhaseSummary) (map[string]PhaseSummary, []PhaseMean) {
		by := make(map[string]PhaseSummary, len(phases))
		means := make([]PhaseMean, len(phases))
		for i, p := range phases {
			by[p.Phase] = p
			means[i] = p.mean()
		}
		return by, means
	}
	byA, meansA := index(a)
	byB, meansB := index(b)
	answeredA, answeredB := answeredBy(meansA), answeredBy(meansB)

	var rows []PhaseRow
	for _, name := range runner.PhaseNames {
		pa, inA := byA[name]
		pb, inB := byB[name]
		if !inA && !inB {
			continue
		}
		r := PhaseRow{
			Phase:       name,
			PerRequestA: pa.mean().perRequestMs(answeredA), PerRequestB: pb.mean().perRequestMs(answeredB),
			P99A: pa.P99Ms, P99B: pb.P99Ms,
			SamplesA: pa.Count, SamplesB: pb.Count,
			ShareA: pa.SharePct, ShareB: pb.SharePct,
		}
		r.P99ChangePct = pct(r.P99A, r.P99B)
		if r.SamplesA == 0 || r.SamplesB == 0 {
			r.P99ChangePct = math.NaN()
		}
		rows = append(rows, r)
	}
	return rows
}

// meanLatencies returns the mean time an answered request spent in all phases,
// in each run: the mean latency of its answered requests.
func (c Comparison) meanLatencies() (a, b float64) {
	for _, r := range c.Phases {
		a += r.PerRequestA
		b += r.PerRequestB
	}
	return a, b
}

// MainPhase is the phase whose time per request changed most, up or down, and
// false if there is none or none changed by PhaseMoveFraction of A's mean.
func (c Comparison) MainPhase() (PhaseRow, bool) {
	meanA, _ := c.meanLatencies()
	var best PhaseRow
	found := false
	for _, r := range c.Phases {
		if !found || math.Abs(r.ChangeMs()) > math.Abs(best.ChangeMs()) {
			best, found = r, true
		}
	}
	if !found || meanA <= 0 || math.Abs(best.ChangeMs()) < PhaseMoveFraction*meanA {
		return PhaseRow{}, false
	}
	return best, true
}

// fmtSignedMs formats a change in milliseconds with its sign: "+1.5ms",
// "-200µs", and "0ns" for none.
func fmtSignedMs(v float64) string {
	switch {
	case v > 0:
		return "+" + FormatMs(v)
	case v < 0:
		return "-" + FormatMs(-v)
	}
	return FormatMs(0)
}

// writePhaseDiff prints one row per phase, and what moved most.
func writePhaseDiff(b *strings.Builder, c Comparison) {
	if len(c.Phases) == 0 {
		return
	}
	p99 := func(m float64, samples uint64) string {
		if samples == 0 {
			return "-"
		}
		return FormatMs(m)
	}
	b.WriteString("\nwhere the time goes, per answered request (corrected latency by phase; p99 is over the requests that had the phase)\n")
	fmt.Fprintf(b, "  %-8s  %8s  %8s  %9s  %8s  %8s  %10s  %7s  %7s\n", "phase", "mean A", "mean B", "change", "p99 A", "p99 B", "p99 change", "share A", "share B")
	for _, r := range c.Phases {
		change := "n/a"
		if !math.IsNaN(r.P99ChangePct) {
			change = fmtPct(r.P99ChangePct)
		}
		fmt.Fprintf(b, "  %-8s  %8s  %8s  %9s  %8s  %8s  %10s  %7s  %7s\n", r.Phase,
			FormatMs(r.PerRequestA), FormatMs(r.PerRequestB), fmtSignedMs(r.ChangeMs()),
			p99(r.P99A, r.SamplesA), p99(r.P99B, r.SamplesB), change,
			fmt.Sprintf("%.1f%%", r.ShareA), fmt.Sprintf("%.1f%%", r.ShareB))
	}
	meanA, meanB := c.meanLatencies()
	if r, ok := c.MainPhase(); ok {
		fmt.Fprintf(b, "biggest change: %s, %s per answered request (mean %s to %s)\n", r.Phase, fmtSignedMs(r.ChangeMs()), FormatMs(meanA), FormatMs(meanB))
	} else {
		fmt.Fprintf(b, "no phase moved by as much as %.0f%% of A's mean (%s)\n", PhaseMoveFraction*100, FormatMs(meanA))
	}
}
