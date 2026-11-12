package runner

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/optimm/Gmeter/internal/hist"
)

// MaxLabels is the most distinct Request.Label values a run reports on. Each one
// keeps two latency histograms of about a quarter of a megabyte, so the limit
// keeps a trace with thousands of different labels from using gigabytes.
const MaxLabels = 100

// LabelResult is what happened to the requests that carry one Request.Label:
// for a replay, the requests to one endpoint.
type LabelResult struct {
	Label     string
	Scheduled uint64 // requests dispatched to workers
	Failed    uint64 // transport failures
	BadStatus uint64 // responses outside 200-399
	// Corrected and Service are as in Result, for this label's requests only.
	// Under load the two differ per label: a slow label holds workers, so the
	// requests of other labels queue behind it, and only their corrected latency
	// shows the wait.
	Corrected *hist.Histogram
	Service   *hist.Histogram
}

// indexLabels sets each request's label index and returns the distinct labels
// in the order they first appear. Requests either all have a label or none do.
func indexLabels(reqs []Request) ([]string, error) {
	if len(reqs) == 0 || reqs[0].Label == "" {
		for i, r := range reqs {
			if r.Label != "" {
				return nil, fmt.Errorf("runner: request %d has a Label but request 1 has none: label every request or none", i+1)
			}
		}
		return nil, nil
	}
	index := map[string]int{}
	var labels []string
	for i := range reqs {
		r := &reqs[i]
		if r.Label == "" {
			return nil, fmt.Errorf("runner: request %d has no Label but request 1 does: label every request or none", i+1)
		}
		n, seen := index[r.Label]
		if !seen {
			n = len(labels)
			index[r.Label] = n
			labels = append(labels, r.Label)
		}
		r.label = n
	}
	if len(labels) > MaxLabels {
		return nil, fmt.Errorf("runner: the requests have %d distinct labels, more than the %d a run reports on", len(labels), MaxLabels)
	}
	return labels, nil
}

// labelResults gathers the per-label figures, busiest label first and equally
// busy ones in label order. The workers must have exited.
func labelResults(labels []string, scheduled []uint64, tallies []partTally) []LabelResult {
	out := make([]LabelResult, len(labels))
	for i, name := range labels {
		t := &tallies[i]
		r := LabelResult{
			Label:     name,
			Scheduled: scheduled[i],
			Failed:    t.failed,
			BadStatus: t.badStatus,
			Corrected: t.corrected,
			Service:   t.service,
		}
		if r.Corrected == nil { // no response was ever recorded for this label
			r.Corrected, r.Service = hist.New(), hist.New()
		}
		out[i] = r
	}
	slices.SortFunc(out, func(a, b LabelResult) int {
		return cmp.Or(cmp.Compare(b.Scheduled, a.Scheduled), cmp.Compare(a.Label, b.Label))
	})
	return out
}
