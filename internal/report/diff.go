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
	c.Rows = []Row{
		row("achieved rps", a.AchievedRPS, b.AchievedRPS, func(v float64) string { return fmt.Sprintf("%.1f", v) }),
		row("failed", float64(a.Failed), float64(b.Failed), fmtCount),
		row("bad status", float64(a.BadStatus), float64(b.BadStatus), fmtCount),
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

func fmtMs(v float64) string {
	return FormatDuration(time.Duration(v * float64(time.Millisecond)))
}
