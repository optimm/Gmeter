package report

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
)

// Comparison is run B measured against run A. Latencies are the corrected ones.
type Comparison struct {
	// Notes lists reasons the two runs may not be like-for-like, or that one of
	// them may not be trustworthy. An empty list does not prove they are
	// comparable, only that nothing recorded says otherwise.
	Notes []string

	Rows    []Row // throughput and failures
	Latency []Row // corrected latency percentiles

	// Shifts answers a question percentiles alone cannot: what share of B's
	// requests are slower than A's p50, p90 and p99? It needs the full
	// histograms, which is why they are saved.
	Shifts []Shift

	// P99ChangePct is the change of corrected p99 from A to B, in percent (NaN
	// if A's p99 is zero). Regressed gates on it.
	P99ChangePct float64

	// Endpoints compares the endpoints both runs have (see Summary.Endpoints),
	// worst p99 increase first; OnlyA and OnlyB name those just one run has.
	Endpoints    []EndpointRow
	OnlyA, OnlyB []string

	// Phases compares where the time of the requests that got a response went,
	// in the order a request goes through the phases (see runner.PhaseNames).
	Phases []PhaseRow

	a, b Summary
}

// Row is one metric in both runs. A, B and Change are already formatted.
type Row struct {
	Label, A, B, Change string
}

// Shift is the share of B's requests slower than A's Quantile-th percentile.
type Shift struct {
	Quantile float64
	Value    time.Duration // A's percentile
	Share    float64       // 0..1, of B's requests slower than Value
}

// Compare measures b against a. It fails if either run's histogram cannot be
// rebuilt.
func Compare(a, b Summary) (Comparison, error) {
	ha, err := hist.FromDump(a.Histograms.Corrected)
	if err != nil {
		return Comparison{}, fmt.Errorf("report: run A: %w", err)
	}
	hb, err := hist.FromDump(b.Histograms.Corrected)
	if err != nil {
		return Comparison{}, fmt.Errorf("report: run B: %w", err)
	}

	c := Comparison{
		a: a, b: b,
		Notes:        likeForLike(a, b),
		P99ChangePct: pct(a.Corrected.P99Ms, b.Corrected.P99Ms),
	}
	switch ea, eb := len(a.Endpoints) > 0, len(b.Endpoints) > 0; {
	case ea && eb:
		c.Endpoints, c.OnlyA, c.OnlyB = compareEndpoints(a.Endpoints, b.Endpoints)
	case ea != eb:
		has := "A"
		if eb {
			has = "B"
		}
		c.Notes = append(c.Notes, fmt.Sprintf("only %s has a per-endpoint breakdown, so endpoints are not compared", has))
	}
	switch pa, pb := len(a.Phases) > 0, len(b.Phases) > 0; {
	case pa && pb:
		c.Phases = comparePhases(a.Phases, b.Phases)
	case pa != pb:
		has := "A"
		if pb {
			has = "B"
		}
		c.Notes = append(c.Notes, fmt.Sprintf("only %s has phase timings, so phases are not compared", has))
	}
	c.Rows = []Row{
		row("achieved rps", a.AchievedRPS, b.AchievedRPS, func(v float64) string { return fmt.Sprintf("%.1f", v) }),
		row("failed", float64(a.Failed), float64(b.Failed), fmtCount),
		row("bad status", float64(a.BadStatus), float64(b.BadStatus), fmtCount),
		c.errorRateRow(),
	}
	c.Latency = []Row{
		row("p50", a.Corrected.P50Ms, b.Corrected.P50Ms, fmtMs),
		row("p90", a.Corrected.P90Ms, b.Corrected.P90Ms, fmtMs),
		row("p99", a.Corrected.P99Ms, b.Corrected.P99Ms, fmtMs),
		row("p99.9", a.Corrected.P999Ms, b.Corrected.P999Ms, fmtMs),
		row("max", a.Corrected.MaxMs, b.Corrected.MaxMs, fmtMs),
	}
	for _, q := range []float64{0.5, 0.9, 0.99} {
		v := ha.Quantile(q)
		c.Shifts = append(c.Shifts, Shift{Quantile: q, Value: v, Share: hb.FractionAbove(v)})
	}
	return c, nil
}

// Regressed reports whether B's corrected p99 is more than thresholdPct percent
// above A's. It deliberately looks only at p99: one number to gate a build on.
func (c Comparison) Regressed(thresholdPct float64) bool {
	return !math.IsNaN(c.P99ChangePct) && c.P99ChangePct > thresholdPct
}

// ErrorRise is how much B's error rate is above A's, in percentage points (a
// fall is negative). The error rate of a run is the share of the requests it
// sent that failed or got a bad status, as in `gmeter find`. It is false when
// either run sent nothing, so has no rate.
func (c Comparison) ErrorRise() (points float64, ok bool) {
	a, b, ok := c.ErrorPercents()
	return b - a, ok
}

// ErrorPercents are the error rates of A and B, in percent; false if either
// run sent nothing.
func (c Comparison) ErrorPercents() (a, b float64, ok bool) {
	a, okA := c.a.errorPct()
	b, okB := c.b.errorPct()
	return a, b, okA && okB
}

// ErrorsRegressed reports whether B's error rate is more than thresholdPoints
// percentage points above A's.
func (c Comparison) ErrorsRegressed(thresholdPoints float64) bool {
	rise, ok := c.ErrorRise()
	return ok && rise > thresholdPoints
}

// errorPct is the share of the requests sent that failed or got a bad status,
// in percent; false if none were sent.
func (s Summary) errorPct() (float64, bool) {
	if s.Scheduled == 0 {
		return 0, false
	}
	return float64(s.Failed+s.BadStatus) * 100 / float64(s.Scheduled), true
}

// WriteText prints the comparison; nameA and nameB label the two runs.
func (c Comparison) WriteText(w io.Writer, nameA, nameB string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "A  %s   %s\n", nameA, runLine(c.a))
	fmt.Fprintf(&b, "B  %s   %s\n", nameB, runLine(c.b))
	for _, n := range c.Notes {
		fmt.Fprintf(&b, "note  %s\n", n)
	}

	table := func(indent string, rows []Row) {
		for _, r := range rows {
			fmt.Fprintf(&b, "%-16s %12s %12s %9s\n", indent+r.Label, r.A, r.B, r.Change)
		}
	}
	fmt.Fprintf(&b, "\n%-16s %12s %12s %9s\n", "", "A", "B", "change")
	table("", c.Rows)
	b.WriteString("latency, corrected\n")
	table("  ", c.Latency)

	b.WriteString("\nshare of B's requests that are slower than A's percentiles\n")
	for _, s := range c.Shifts {
		fmt.Fprintf(&b, "  slower than A's p%.4g (%s): %.1f%%   (at most %.1f%% if nothing changed)\n",
			s.Quantile*100, FormatDuration(s.Value), s.Share*100, (1-s.Quantile)*100)
	}
	writePhaseDiff(&b, c)
	writeEndpointDiff(&b, c)

	_, err := io.WriteString(w, b.String())
	return err
}

// likeForLike lists differences in how the runs were set up, and any warning a
// run raised about its own generator.
func likeForLike(a, b Summary) []string {
	var notes []string
	differs := func(what string, x, y any) {
		notes = append(notes, fmt.Sprintf("%s differs (A: %v, B: %v): this is not a like-for-like comparison", what, x, y))
	}
	if a.Run.URL != b.Run.URL {
		differs("URL", a.Run.URL, b.Run.URL)
	}
	if a.Run.Method != b.Run.Method {
		differs("method", a.Run.Method, b.Run.Method)
	}
	rateOrDuration := false // whether the notes below already say the load differs
	if a.TargetRPS != b.TargetRPS {
		differs("target rate", fmt.Sprintf("%.6g rps", a.TargetRPS), fmt.Sprintf("%.6g rps", b.TargetRPS))
		rateOrDuration = true
	}
	if a.Run.Workers != b.Run.Workers {
		differs("workers", a.Run.Workers, b.Run.Workers)
	}
	if a.Run.DurationSeconds != b.Run.DurationSeconds {
		differs("duration", fmt.Sprintf("%gs", a.Run.DurationSeconds), fmt.Sprintf("%gs", b.Run.DurationSeconds))
		rateOrDuration = true
	}
	// A step and a flat run can share a duration and an average rate. A file
	// from before profiles were recorded has none, so there is nothing to compare.
	if a.Run.Profile != "" && b.Run.Profile != "" && a.Run.Profile != b.Run.Profile && !rateOrDuration {
		differs("load profile", a.Run.Profile, b.Run.Profile)
	}
	switch aa, ab := arrivalsOf(a), arrivalsOf(b); {
	case aa != ab:
		differs("arrivals", aa, ab)
	case aa == "poisson" && a.Run.Seed != b.Run.Seed:
		notes = append(notes, fmt.Sprintf("Poisson seed differs (A: %d, B: %d): the runs saw different bursts, so expect more run-to-run noise than usual", a.Run.Seed, b.Run.Seed))
	}
	if ra, rb := replayText(a.Run.Replay), replayText(b.Run.Replay); ra != rb {
		differs("replay", ra, rb)
	}
	if a.Run.SpinSeconds != b.Run.SpinSeconds {
		differs("pacing spin", spinText(a.Run.SpinSeconds), spinText(b.Run.SpinSeconds))
	}
	for _, r := range []struct {
		name string
		s    Summary
	}{{"A", a}, {"B", b}} {
		if len(r.s.Warnings) > 0 {
			notes = append(notes, fmt.Sprintf("%s reported %d warning(s) about the generator (see its summary): its latencies may be inflated", r.name, len(r.s.Warnings)))
		}
	}
	return notes
}

// arrivalsOf is a run's arrival process. Files from before it was recorded were
// all evenly spaced.
func arrivalsOf(s Summary) string {
	if s.Run.Arrivals == "" {
		return "even"
	}
	return s.Run.Arrivals
}

// replayText describes what a run replayed, for comparing two runs: "none" for
// a run that sent one request over and over.
func replayText(r *ReplayInfo) string {
	if r == nil {
		return "none"
	}
	if r.Timing == "recorded" {
		return fmt.Sprintf("%s, %d requests, recorded timing at %gx", r.Source, r.Requests, r.Speed)
	}
	return fmt.Sprintf("%s, %d requests, fixed rate", r.Source, r.Requests)
}

// spinText names a pacing spin window; zero means the plain timer was used,
// which is also what a file from before the setting existed implies.
func spinText(seconds float64) string {
	if seconds == 0 {
		return "off"
	}
	return FormatDuration(time.Duration(math.Round(seconds * 1e9)))
}

func runLine(s Summary) string {
	if s.Run.URL == "" {
		return "(no run info)"
	}
	return strings.TrimSpace(fmt.Sprintf("%s %s   %g rps   %gs   %d workers   %s",
		s.Run.Method, s.Run.URL, s.TargetRPS, s.Run.DurationSeconds, s.Run.Workers, s.Run.StartedAt))
}

func row(label string, a, b float64, format func(float64) string) Row {
	return Row{Label: label, A: format(a), B: format(b), Change: fmtPct(pct(a, b))}
}

// pct is the change from a to b in percent: 0 if both are zero, NaN if only a
// is zero (there is no baseline to be relative to).
func pct(a, b float64) float64 {
	switch {
	case a == 0 && b == 0:
		return 0
	case a == 0:
		return math.NaN()
	}
	return (b - a) / a * 100
}

func fmtPct(p float64) string {
	if math.IsNaN(p) {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", p)
}

func fmtCount(v float64) string { return fmt.Sprintf("%.0f", v) }

func fmtMs(v float64) string { return FormatMs(v) }

// FormatMs formats a latency given in milliseconds as FormatDuration would.
func FormatMs(v float64) string {
	return FormatDuration(time.Duration(v * float64(time.Millisecond)))
}

// errorRateRow is the error rate of both runs and the change in percentage
// points, which unlike a relative change is defined when A has no errors.
func (c Comparison) errorRateRow() Row {
	show := func(s Summary) string {
		if p, ok := s.errorPct(); ok {
			return fmt.Sprintf("%.1f%%", p)
		}
		return "n/a"
	}
	r := Row{Label: "error rate", A: show(c.a), B: show(c.b), Change: "n/a"}
	if rise, ok := c.ErrorRise(); ok {
		r.Change = fmt.Sprintf("%+.1f points", rise)
	}
	return r
}
