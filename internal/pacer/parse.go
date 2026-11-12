package pacer

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseStages reads a load profile written as comma-separated stages, each
// DURATION@RATE for a constant rate or DURATION@FROM->TO for a linear ramp:
//
//	30s@100                      100 requests/s for 30 seconds
//	30s@100,30s@200,30s@400      a step test
//	10s@50,1m@50->500,30s@500    warm up, ramp, hold
//
// A rate of 0 is a pause. Stage.String writes the same notation back.
func ParseStages(s string) ([]Stage, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("no stages given")
	}
	var stages []Stage
	for i, item := range strings.Split(s, ",") {
		st, err := parseStage(strings.TrimSpace(item))
		if err != nil {
			return nil, fmt.Errorf("stage %d: %w", i+1, err)
		}
		stages = append(stages, st)
	}
	return stages, nil
}

func parseStage(item string) (Stage, error) {
	dur, rates, ok := strings.Cut(item, "@")
	if !ok {
		return Stage{}, fmt.Errorf("%q is not DURATION@RATE or DURATION@FROM->TO", item)
	}
	d, err := time.ParseDuration(strings.TrimSpace(dur))
	if err != nil || d <= 0 {
		return Stage{}, fmt.Errorf("invalid duration %q in %q: want a positive duration such as 30s or 2m", dur, item)
	}
	from, to, isRamp := strings.Cut(rates, "->")
	st := Stage{Duration: d}
	if st.From, err = parseRate(from); err != nil {
		return Stage{}, fmt.Errorf("%w in %q", err, item)
	}
	st.To = st.From
	if isRamp {
		if st.To, err = parseRate(to); err != nil {
			return Stage{}, fmt.Errorf("%w in %q", err, item)
		}
	}
	return st, nil
}

func parseRate(s string) (float64, error) {
	s = strings.TrimSpace(s)
	r, err := strconv.ParseFloat(s, 64)
	if err != nil || r < 0 || math.IsNaN(r) || math.IsInf(r, 0) {
		return 0, fmt.Errorf("invalid rate %q: want a non-negative number of requests per second", s)
	}
	return r, nil
}

// String writes the stage in the notation ParseStages reads.
func (s Stage) String() string {
	rate := func(r float64) string { return strconv.FormatFloat(r, 'f', -1, 64) }
	if s.From == s.To {
		return shortDuration(s.Duration) + "@" + rate(s.From)
	}
	return shortDuration(s.Duration) + "@" + rate(s.From) + "->" + rate(s.To)
}

// shortDuration is Duration.String without the redundant zero units ("1m",
// not "1m0s").
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// ParseArrivals reads an arrival process name: "even" or "poisson".
func ParseArrivals(s string) (Arrivals, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "even":
		return Even, nil
	case "poisson":
		return Poisson, nil
	}
	return Even, fmt.Errorf("unknown arrival process %q: want even or poisson", s)
}
