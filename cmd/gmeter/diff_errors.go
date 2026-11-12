package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/optimm/Gmeter/internal/report"
)

// judgeErrors prints whether B's error rate is more than thresholdPoints
// percentage points above A's, for the whole run and then for each endpoint the
// files break out, and reports whether any is. It is an error when a run sent
// nothing: a gate that compared no rates must not say all is well.
func judgeErrors(c report.Comparison, thresholdPoints float64, w io.Writer) (regressed bool, err error) {
	a, b, ok := c.ErrorPercents()
	if !ok {
		return false, errors.New("a run sent no requests, so it has no error rate to compare")
	}
	rise := b - a
	if c.ErrorsRegressed(thresholdPoints) {
		fmt.Fprintf(w, "\nREGRESSION: error rate rose from %.1f%% to %.1f%% (%+.1f points), over the %g-point threshold\n", a, b, rise, thresholdPoints)
		regressed = true
	} else {
		fmt.Fprintf(w, "\nok: error rate went from %.1f%% to %.1f%% (%+.1f points), within the %g-point threshold\n", a, b, rise, thresholdPoints)
	}
	if len(c.Endpoints) == 0 {
		return regressed, nil
	}
	return judgeEndpointErrors(c, thresholdPoints, w) || regressed, nil
}

// judgeEndpointErrors does the same for each endpoint both runs have. A whole
// run can hide an endpoint that carries a small share of the traffic.
func judgeEndpointErrors(c report.Comparison, thresholdPoints float64, w io.Writer) (regressed bool) {
	judged := c.EndpointsErrorGated()
	if judged == 0 {
		fmt.Fprintf(w, "note: no endpoint was sent %d requests in both runs, so none could be judged for errors\n", report.MinEndpointSamples)
		return false
	}
	points := "points"
	if thresholdPoints == 1 {
		points = "point"
	}
	regs := c.EndpointErrorRegressions(thresholdPoints)
	if len(regs) == 0 {
		detail := fmt.Sprintf("%d judged", judged)
		if skipped := len(c.Endpoints) - judged; skipped > 0 {
			detail += fmt.Sprintf(", %d not judged", skipped)
		}
		fmt.Fprintf(w, "ok: no endpoint's error rate rose more than %g %s (%s)\n", thresholdPoints, points, detail)
		return false
	}
	noun := "endpoints'"
	if len(regs) == 1 {
		noun = "endpoint's"
	}
	fmt.Fprintf(w, "REGRESSION: %d %s error rate rose more than %g %s:\n", len(regs), noun, thresholdPoints, points)
	width := 0
	for _, r := range regs {
		width = max(width, len(r.Endpoint))
	}
	for _, r := range regs {
		a, b, _ := r.ErrorPercents()
		fmt.Fprintf(w, "  %-*s  %.1f%% -> %.1f%%  (%+.1f points)\n", width, r.Endpoint, a, b, b-a)
	}
	return true
}
