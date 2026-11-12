package report

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// MinEndpointSamples is the fewest latency samples an endpoint needs, in each
// run, for a change in its p99 to be judged. The p99 of fewer than a hundred
// samples is nearly the slowest one, so a change in it is mostly noise.
const MinEndpointSamples = 100

// EndpointRow is one endpoint present in both runs. Latencies are corrected, in
// milliseconds.
type EndpointRow struct {
	Endpoint             string
	RequestsA, RequestsB uint64 // dispatched
	SamplesA, SamplesB   uint64 // latency samples; zero if every request failed to get a response
	P50A, P50B           float64
	P99A, P99B           float64
	ErrorsA, ErrorsB     uint64 // failed requests plus bad statuses

	// P99ChangePct is the change from A to B in percent: NaN if there is no
	// measurement to compare (a run had no samples, or A's p99 is zero).
	P99ChangePct float64

	// Few is set when A or B has fewer than MinEndpointSamples samples.
	Few bool

	// Moves is how much the time an answered request of the endpoint spent in each
	// phase changed from A to B, in milliseconds (negative if it fell), one
	// number for each of the phases in shownPhases: queue, connect (dns, connect
	// and tls together), send, wait, transfer. Each run is averaged over its own
	// answered requests. It is nil unless both runs recorded the endpoint's
	// phases.
	Moves []float64
}

// GrewMost is the phase in which an answered request of the endpoint spent the
// most additional time in B, and how many milliseconds more; false if the
// phases were not compared or none got slower.
func (r EndpointRow) GrewMost() (phase string, ms float64, ok bool) {
	for i, m := range r.Moves {
		if m > ms {
			phase, ms, ok = shownPhases[i], m, true
		}
	}
	return phase, ms, ok
}

// compareEndpoints matches the endpoints of two runs by name. The rows are in
// order of the biggest p99 increase, with those that cannot be judged after
// the ones that can; onlyA and onlyB name the endpoints just one run has, in
// that run's order (busiest first).
func compareEndpoints(a, b []EndpointSummary) (rows []EndpointRow, onlyA, onlyB []string) {
	inB := make(map[string]EndpointSummary, len(b))
	for _, e := range b {
		inB[e.Endpoint] = e
	}
	inA := make(map[string]bool, len(a))
	for _, ea := range a {
		inA[ea.Endpoint] = true
		eb, ok := inB[ea.Endpoint]
		if !ok {
			onlyA = append(onlyA, ea.Endpoint)
			continue
		}
		rows = append(rows, endpointRow(ea, eb))
	}
	for _, eb := range b {
		if !inA[eb.Endpoint] {
			onlyB = append(onlyB, eb.Endpoint)
		}
	}

	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	slices.SortFunc(rows, func(x, y EndpointRow) int {
		return cmp.Or(
			cmp.Compare(flag(x.Few), flag(y.Few)),
			cmp.Compare(y.P99ChangePct, x.P99ChangePct), // descending; cmp puts NaN, a change that cannot be measured, below every number
			cmp.Compare(x.Endpoint, y.Endpoint),
		)
	})
	return rows, onlyA, onlyB
}

func endpointRow(a, b EndpointSummary) EndpointRow {
	r := EndpointRow{
		Endpoint:  a.Endpoint,
		RequestsA: a.Scheduled, RequestsB: b.Scheduled,
		SamplesA: a.Corrected.Count, SamplesB: b.Corrected.Count,
		P50A: a.Corrected.P50Ms, P50B: b.Corrected.P50Ms,
		P99A: a.Corrected.P99Ms, P99B: b.Corrected.P99Ms,
		ErrorsA: a.Failed + a.BadStatus, ErrorsB: b.Failed + b.BadStatus,
	}
	if va, answeredA := perRequest(a.Phases); answeredA > 0 {
		if vb, answeredB := perRequest(b.Phases); answeredB > 0 {
			r.Moves = make([]float64, len(shownPhases))
			for i := range r.Moves {
				r.Moves[i] = vb[i] - va[i]
			}
		}
	}
	r.Few = r.SamplesA < MinEndpointSamples || r.SamplesB < MinEndpointSamples
	r.P99ChangePct = pct(r.P99A, r.P99B)
	if r.SamplesA == 0 || r.SamplesB == 0 {
		r.P99ChangePct = math.NaN()
	}
	return r
}

// EndpointRegressions returns the endpoints whose corrected p99 in B is more
// than thresholdPct percent above A's, the biggest increase first. Endpoints
// with fewer than MinEndpointSamples samples in either run, or with nothing to
// compare, are not judged. Like Regressed it looks only at p99.
func (c Comparison) EndpointRegressions(thresholdPct float64) []EndpointRow {
	var out []EndpointRow
	for _, r := range c.Endpoints {
		if !r.Few && !math.IsNaN(r.P99ChangePct) && r.P99ChangePct > thresholdPct {
			out = append(out, r)
		}
	}
	return out
}

// ErrorPercents are the endpoint's error rates in A and B, in percent: failed
// requests plus bad statuses as a share of the requests sent to it. It is false
// when either run sent it fewer than MinEndpointSamples requests, since a rate
// over so few is mostly chance. Unlike the p99 it does not need latency
// samples: an endpoint that fails every request is the case it exists for.
func (r EndpointRow) ErrorPercents() (a, b float64, ok bool) {
	if r.RequestsA < MinEndpointSamples || r.RequestsB < MinEndpointSamples {
		return 0, 0, false
	}
	return float64(r.ErrorsA) * 100 / float64(r.RequestsA), float64(r.ErrorsB) * 100 / float64(r.RequestsB), true
}

// EndpointErrorRegressions lists the endpoints whose error rate in B is more than
// thresholdPoints percentage points above A's, the biggest rise first.
func (c Comparison) EndpointErrorRegressions(thresholdPoints float64) []EndpointRow {
	var out []EndpointRow
	rise := func(r EndpointRow) float64 {
		a, b, _ := r.ErrorPercents()
		return b - a
	}
	for _, r := range c.Endpoints {
		if _, _, ok := r.ErrorPercents(); ok && rise(r) > thresholdPoints {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(x, y EndpointRow) int { return cmp.Compare(rise(y), rise(x)) })
	return out
}

// EndpointsErrorGated is how many endpoints EndpointErrorRegressions can judge.
func (c Comparison) EndpointsErrorGated() int {
	n := 0
	for _, r := range c.Endpoints {
		if _, _, ok := r.ErrorPercents(); ok {
			n++
		}
	}
	return n
}

// SharedRisePct, SharedRiseFraction and SharedRiseMinEndpoints say when a diff
// looks like one shared cause moving every endpoint, not one endpoint's own
// regression: at least SharedRiseMinEndpoints endpoints judged, and at least
// SharedRiseFraction of them with a p99 more than SharedRisePct percent up. They
// are a judgement, not a measurement: in three saved pairs a single slow
// endpoint moved 1 of 3 endpoints, moderate load effects 0 or 1 of 3, and a
// saturated server all 3 (by 5,600 to 8,200%).
const (
	SharedRisePct          = 50.0
	SharedRiseFraction     = 0.75
	SharedRiseMinEndpoints = 3
)

// SharedRise is how many of the endpoints the p99 gate can judge rose by more
// than SharedRisePct percent, and how many it judged; ok is whether that looks
// like one shared cause (see SharedRisePct).
func (c Comparison) SharedRise() (rose, judged int, ok bool) {
	for _, r := range c.Endpoints {
		if r.Few || math.IsNaN(r.P99ChangePct) {
			continue
		}
		judged++
		if r.P99ChangePct > SharedRisePct {
			rose++
		}
	}
	ok = judged >= SharedRiseMinEndpoints && float64(rose) >= SharedRiseFraction*float64(judged)
	return rose, judged, ok
}

// EndpointsGated is how many endpoints EndpointRegressions can judge. Zero means
// a gate on endpoints would pass whatever happened, which is not the same as
// the endpoints being fine.
func (c Comparison) EndpointsGated() int {
	n := 0
	for _, r := range c.Endpoints {
		if !r.Few && !math.IsNaN(r.P99ChangePct) {
			n++
		}
	}
	return n
}

// writeEndpointDiff prints the endpoint table. Only one endpoint present in
// both runs would just repeat the totals, so then it prints nothing, unless
// endpoints are missing from one of the runs, which is worth saying.
func writeEndpointDiff(b *strings.Builder, c Comparison) {
	if len(c.Endpoints) < 2 && len(c.OnlyA)+len(c.OnlyB) == 0 {
		return
	}
	d := func(m float64, samples uint64) string {
		if samples == 0 {
			return "-"
		}
		return fmtMs(m)
	}
	change := func(r EndpointRow) string {
		if math.IsNaN(r.P99ChangePct) {
			return "n/a"
		}
		return fmtPct(r.P99ChangePct)
	}

	b.WriteString("\nendpoints, worst p99 change first (corrected latency; errors are failures and bad statuses, A to B)\n")
	if len(c.Endpoints) > 0 {
		width := len("endpoint")
		for _, r := range c.Endpoints {
			width = max(width, min(utf8.RuneCountInString(r.Endpoint), endpointWidth))
		}
		fmt.Fprintf(b, "  %-*s  %6s  %6s  %8s  %8s  %8s  %8s  %10s  %9s\n", width, "endpoint", "req A", "req B", "p50 A", "p50 B", "p99 A", "p99 B", "p99 change", "errors")
		few := false
		for _, r := range c.Endpoints {
			mark := ""
			if r.Few {
				mark, few = " *", true
			}
			fmt.Fprintf(b, "  %-*s  %6d  %6d  %8s  %8s  %8s  %8s  %10s  %9s%s\n", width, shorten(r.Endpoint, endpointWidth),
				r.RequestsA, r.RequestsB, d(r.P50A, r.SamplesA), d(r.P50B, r.SamplesB), d(r.P99A, r.SamplesA), d(r.P99B, r.SamplesB),
				change(r), fmt.Sprintf("%d→%d", r.ErrorsA, r.ErrorsB), mark)
		}
		if few {
			fmt.Fprintf(b, "* fewer than %d requests answered in A or B: its p99 is close to its maximum, so the change is mostly noise, and -endpoint-threshold does not judge it\n", MinEndpointSamples)
		}
	}
	only := func(run string, names []string, all []EndpointSummary) {
		if len(names) == 0 {
			return
		}
		parts := make([]string, len(names))
		for i, name := range names {
			for _, e := range all {
				if e.Endpoint == name {
					parts[i] = fmt.Sprintf("%s (%d requests)", shorten(name, endpointWidth), e.Scheduled)
				}
			}
		}
		fmt.Fprintf(b, "only in %s: %s\n", run, strings.Join(parts, ", "))
	}
	only("A", c.OnlyA, c.a.Endpoints)
	only("B", c.OnlyB, c.b.Endpoints)
	writeEndpointMoves(b, c)
}

// writeEndpointMoves prints, for each endpoint both runs recorded the phases of,
// how much the time an answered request spent in each phase changed.
func writeEndpointMoves(b *strings.Builder, c Comparison) {
	var rows []EndpointRow
	width := len("endpoint")
	for _, r := range c.Endpoints {
		if r.Moves != nil {
			rows = append(rows, r)
			width = max(width, min(utf8.RuneCountInString(r.Endpoint), endpointWidth))
		}
	}
	if len(rows) == 0 {
		return
	}
	b.WriteString("\nendpoints, where the time moved (change in the mean time per answered request, B minus A; connect is dns + connect + tls)\n")
	fmt.Fprintf(b, "  %-*s", width, "endpoint")
	for _, name := range shownPhases {
		fmt.Fprintf(b, "  %9s", name)
	}
	b.WriteString("\n")
	few := false
	for _, r := range rows {
		mark := ""
		if r.Few {
			mark, few = " *", true
		}
		fmt.Fprintf(b, "  %-*s", width, shorten(r.Endpoint, endpointWidth))
		for _, m := range r.Moves {
			fmt.Fprintf(b, "  %9s", fmtSignedMs(m))
		}
		b.WriteString(mark + "\n")
	}
	if few {
		fmt.Fprintf(b, "* fewer than %d requests answered in A or B: a mean of so few requests is a noisy estimate\n", MinEndpointSamples)
	}
}
