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
