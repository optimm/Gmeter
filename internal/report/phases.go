package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// PhaseSummary is the time requests spent in one phase of their corrected
// latency (see runner.PhaseNames for what each phase is).
type PhaseSummary struct {
	Phase string `json:"phase"`
	// Latency digests the phase over the requests that had it: queue, send, wait
	// and transfer every request that got a response, dns, connect and tls only
	// those that opened a connection.
	Latency
	// SharePct is this phase's part of the corrected latency of all requests that
	// got a response, in percent: the phases' shares add up to 100. A phase that
	// only some requests have is counted over all of them, so a rare but slow
	// connect has a small share.
	SharePct float64 `json:"share_pct"`
}

// phaseSummaries digests the phases, leaving out those no request went through,
// or returns nil when there are none.
func phaseSummaries(phases []runner.PhaseResult) []PhaseSummary {
	var total time.Duration
	for _, p := range phases {
		total += p.Samples.Sum()
	}
	var out []PhaseSummary
	for _, p := range phases {
		if p.Samples.Count() == 0 {
			continue
		}
		share := 0.0
		if total > 0 {
			share = float64(p.Samples.Sum()) / float64(total) * 100
		}
		out = append(out, PhaseSummary{Phase: p.Phase, Latency: digest(p.Samples), SharePct: share})
	}
	return out
}

// writePhases prints one row per phase.
func writePhases(b *strings.Builder, phases []PhaseSummary) {
	if len(phases) == 0 {
		return
	}
	d := func(m float64) string { return fmtMs(m) }
	b.WriteString("\nwhere the time goes (corrected latency of the requests that got a response, by phase)\n")
	fmt.Fprintf(b, "  %-8s  %8s  %8s  %8s  %8s  %6s\n", "phase", "requests", "p50", "p99", "max", "share")
	connection := false
	for _, p := range phases {
		fmt.Fprintf(b, "  %-8s  %8d  %8s  %8s  %8s  %6s\n", p.Phase, p.Count, d(p.P50Ms), d(p.P99Ms), d(p.MaxMs), fmt.Sprintf("%.1f%%", p.SharePct))
		connection = connection || p.Phase == "dns" || p.Phase == "connect" || p.Phase == "tls"
	}
	b.WriteString("  queue: from the due time until sent (waiting for a free worker, or a late timer)\n")
	b.WriteString("  send: taking a connection from the pool and writing the request   wait: request written to first byte\n")
	b.WriteString("  transfer: first byte to the end of the body\n")
	if connection {
		b.WriteString("  dns, connect and tls only count requests that opened a connection\n")
	}
}

// PhaseMean is where one endpoint's time went in one phase. An endpoint keeps
// only means, not the distributions of PhaseSummary: a distribution costs a
// quarter of a megabyte, and a trace can have a hundred endpoints.
type PhaseMean struct {
	Phase string `json:"phase"`
	// Count is how many of the endpoint's answered requests had the phase: all of
	// them for every phase but dns, connect and tls.
	Count uint64 `json:"count"`
	// MeanMs is the average time in the phase of those Count requests.
	MeanMs float64 `json:"mean_ms"`
	// SharePct is the phase's part of the time the endpoint's answered requests
	// took, in percent; the shares add up to 100.
	SharePct float64 `json:"share_pct"`
}

// phaseMeans digests one endpoint's phase totals, leaving out phases none of its
// requests went through, or returns nil if there are none.
func phaseMeans(totals []runner.PhaseTotal) []PhaseMean {
	var all time.Duration
	for _, t := range totals {
		all += t.Total
	}
	var out []PhaseMean
	for _, t := range totals {
		if t.Requests == 0 {
			continue
		}
		share := 0.0
		if all > 0 {
			share = float64(t.Total) / float64(all) * 100
		}
		out = append(out, PhaseMean{
			Phase:    t.Phase,
			Count:    t.Requests,
			MeanMs:   float64(t.Total) / float64(t.Requests) / float64(time.Millisecond),
			SharePct: share,
		})
	}
	return out
}

// shownPhases are the columns of the per-endpoint tables: dns, connect and tls
// share one, as which of them a request has depends on its URL, not on its
// endpoint, and each is a rare event on average.
var shownPhases = [...]string{"queue", "connect", "send", "wait", "transfer"}

// shownIndex is the column of a phase in shownPhases.
func shownIndex(phase string) int {
	switch phase {
	case "queue":
		return 0
	case "dns", "connect", "tls":
		return 1
	case "send":
		return 2
	case "wait":
		return 3
	}
	return 4 // transfer
}

// answeredBy is how many requests the phases are of: every request that got a
// response has a queue phase. It is zero if there are no phases.
func answeredBy(phases []PhaseMean) uint64 {
	for _, p := range phases {
		if p.Phase == "queue" {
			return p.Count
		}
	}
	return 0
}

// perRequestMs is the time a request of the answered ones spent in this phase,
// on average. A phase only some requests had counts as zero for the rest, so
// across the phases these add up to the mean latency of the answered requests.
func (p PhaseMean) perRequestMs(answered uint64) float64 {
	if answered == 0 {
		return 0
	}
	return float64(p.Count) * p.MeanMs / float64(answered)
}

// perRequest is the time an answered request of this endpoint spent in each of
// shownPhases, on average (see PhaseMean.perRequestMs); answered is how many
// requests that is. Both are zero if there are no phases.
func perRequest(phases []PhaseMean) (v [len(shownPhases)]float64, answered uint64) {
	answered = answeredBy(phases)
	for _, p := range phases {
		v[shownIndex(p.Phase)] += p.perRequestMs(answered)
	}
	return v, answered
}

// mean is the run's phase seen as an endpoint's: its count, mean and share.
func (p PhaseSummary) mean() PhaseMean {
	return PhaseMean{Phase: p.Phase, Count: p.Count, MeanMs: p.MeanMs, SharePct: p.SharePct}
}
