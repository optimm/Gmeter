package report

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimm/Gmeter/internal/runner"
)

// EndpointSummary is one endpoint of a replayed trace: the requests that were
// sent to it, wherever in the run they were due.
type EndpointSummary struct {
	Endpoint  string  `json:"endpoint"` // e.g. "GET /orders/:id"; "(other)" for those beyond the busiest
	Scheduled uint64  `json:"scheduled"`
	Failed    uint64  `json:"failed"`
	BadStatus uint64  `json:"bad_status"`
	Corrected Latency `json:"latency_corrected"`
	Service   Latency `json:"latency_service"`
}

// endpointSummaries digests the per-label results, or returns nil when the run
// had none.
func endpointSummaries(labels []runner.LabelResult) []EndpointSummary {
	if len(labels) == 0 {
		return nil
	}
	out := make([]EndpointSummary, len(labels))
	for i, l := range labels {
		out[i] = EndpointSummary{
			Endpoint:  l.Label,
			Scheduled: l.Scheduled,
			Failed:    l.Failed,
			BadStatus: l.BadStatus,
			Corrected: digest(l.Corrected),
			Service:   digest(l.Service),
		}
	}
	return out
}

// endpointWidth is the longest endpoint name the text report prints in full.
const endpointWidth = 36

// writeEndpoints prints one row per endpoint. A single endpoint would only
// repeat the run's own numbers, so then it prints nothing.
func writeEndpoints(b *strings.Builder, endpoints []EndpointSummary) {
	if len(endpoints) < 2 {
		return
	}
	width := len("endpoint")
	for _, e := range endpoints {
		width = max(width, min(utf8.RuneCountInString(e.Endpoint), endpointWidth))
	}
	d := func(m float64) string { return FormatDuration(time.Duration(m * float64(time.Millisecond))) }

	b.WriteString("\nendpoints, busiest first (corrected latency; svc p99 is service latency, from when each request was actually sent)\n")
	fmt.Fprintf(b, "  %-*s  %9s  %6s  %6s  %8s  %8s  %8s  %8s\n", width, "endpoint", "scheduled", "bad", "failed", "p50", "p99", "max", "svc p99")
	for _, e := range endpoints {
		p50, p99, worst, svc := "-", "-", "-", "-"
		if e.Corrected.Count > 0 {
			p50, p99, worst, svc = d(e.Corrected.P50Ms), d(e.Corrected.P99Ms), d(e.Corrected.MaxMs), d(e.Service.P99Ms)
		}
		fmt.Fprintf(b, "  %-*s  %9d  %6d  %6d  %8s  %8s  %8s  %8s\n",
			width, shorten(e.Endpoint, endpointWidth), e.Scheduled, e.BadStatus, e.Failed, p50, p99, worst, svc)
	}
}

// shorten cuts s to at most width characters, ending in an ellipsis if it was cut.
func shorten(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	return string([]rune(s)[:width-1]) + "…"
}
