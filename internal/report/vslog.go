package report

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/optimm/Gmeter/internal/runner"
)

// LogDifferWarnFraction is the share of compared responses that may be of another
// class than logged before the report warns. A few are normal (a record deleted
// since), and the line shows the exact figure either way; a twentieth is where
// the target stops looking like the server the log came from.
const LogDifferWarnFraction = 0.05

// changesShown is how many kinds of change the text report names before it
// counts the rest.
const changesShown = 4

// VsLog is how the responses of a replay compare with the statuses its traffic
// log recorded. Only responses to requests that had a logged status are
// compared, and by class: a 204 where the log has a 200 is the same answer.
type VsLog struct {
	Compared  uint64      `json:"compared"`
	Different uint64      `json:"different"`
	Changes   []LogChange `json:"changes"` // most common first
}

// LogChange is a number of responses of class Got ("4xx") where the log has class Logged.
type LogChange struct {
	Logged string `json:"logged"`
	Got    string `json:"got"`
	Count  uint64 `json:"count"`
}

func (c LogChange) String() string { return fmt.Sprintf("%s→%s %d", c.Logged, c.Got, c.Count) }

// vsLog digests the runner's comparison; nil when nothing was compared.
func vsLog(l runner.LoggedStatuses) *VsLog {
	if l.Compared == 0 {
		return nil
	}
	v := &VsLog{Compared: l.Compared, Different: l.Differ(), Changes: make([]LogChange, len(l.Changes))}
	for i, c := range l.Changes {
		v.Changes[i] = LogChange{Logged: fmt.Sprintf("%dxx", c.Logged), Got: fmt.Sprintf("%dxx", c.Got), Count: c.Count}
	}
	return v
}

func (v *VsLog) differFraction() float64 { return float64(v.Different) / float64(v.Compared) }

// changesText lists the most common changes, and how many kinds are left out.
func (v *VsLog) changesText() string {
	parts := make([]string, 0, changesShown+1)
	for i, c := range v.Changes {
		if i == changesShown {
			parts = append(parts, fmt.Sprintf("and %d more", len(v.Changes)-changesShown))
			break
		}
		parts = append(parts, c.String())
	}
	return strings.Join(parts, ", ")
}

// warning is what to say when too many responses differ from the log, or "".
func (v *VsLog) warning() string {
	if v == nil || v.differFraction() < LogDifferWarnFraction {
		return ""
	}
	return fmt.Sprintf("%d of %d responses (%.0f%%) were of a different class than the log recorded (%s): the target may not hold the data the log asked for, so these latencies may not be what production would see; check before reading them",
		v.Different, v.Compared, v.differFraction()*100, v.changesText())
}

func writeVsLog(b *strings.Builder, v *VsLog) {
	if v == nil {
		return
	}
	fmt.Fprintf(b, "vs log     %d compared, %d differ (%.1f%%)", v.Compared, v.Different, v.differFraction()*100)
	if v.Different > 0 {
		fmt.Fprintf(b, ": %s", v.changesText())
	}
	b.WriteString("\n")
}

// writeEndpointVsLog lists the endpoints whose responses differed from the log,
// the most different first. One endpoint would only repeat the run's line.
func writeEndpointVsLog(b *strings.Builder, endpoints []EndpointSummary) {
	if len(endpoints) < 2 {
		return
	}
	var differing []EndpointSummary
	for _, e := range endpoints {
		if e.LogDifferent > 0 {
			differing = append(differing, e)
		}
	}
	if len(differing) == 0 {
		return
	}
	share := func(e EndpointSummary) float64 { return float64(e.LogDifferent) / float64(e.LogCompared) }
	slices.SortFunc(differing, func(x, y EndpointSummary) int {
		return cmp.Or(cmp.Compare(share(y), share(x)), cmp.Compare(y.LogDifferent, x.LogDifferent), cmp.Compare(x.Endpoint, y.Endpoint))
	})
	b.WriteString("\nendpoints, status vs the log (responses of another class than the log's)\n")
	width := len("endpoint")
	for _, e := range differing {
		width = max(width, len(e.Endpoint))
	}
	fmt.Fprintf(b, "  %-*s  %12s  %7s\n", width, "endpoint", "differ", "share")
	for _, e := range differing {
		fmt.Fprintf(b, "  %-*s  %12s  %6.1f%%\n", width, e.Endpoint, fmt.Sprintf("%d of %d", e.LogDifferent, e.LogCompared), share(e)*100)
	}
}
