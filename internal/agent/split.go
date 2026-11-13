package agent

import (
	"errors"
	"fmt"

	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

// Split divides j into n jobs, one per agent, that together send what j
// would on one machine:
//
//   - A profile is run by every agent at 1/n of its rate. With even arrivals
//     agent i is delayed by i/n of a request (Job.Phase), so the agents take
//     turns exactly as one generator's requests would follow each other,
//     instead of all firing at once. With random arrivals agent i draws from
//     seed+i; n independent random streams at 1/n of the rate add up to one
//     at the whole rate.
//   - A recorded trace is dealt out: agent i sends requests i, n+i, 2n+i, ...
//     at their recorded times, and every agent's replay lasts as long as the
//     whole trace (Job.Span), so that their results line up.
//   - A trace cycled at a fixed rate is reordered so that agent i's k-th
//     request is the whole test's (n*k+i)-th, as it is with even arrivals.
//   - The workers are shared out.
//
// The agents' results, merged with runner.Merge, are then the result of j.
func (j Job) Split(n int) ([]Job, error) {
	switch {
	case n < 1:
		return nil, fmt.Errorf("cannot split a test among %d agents", n)
	case j.Workers < n:
		return nil, fmt.Errorf("%d workers cannot be shared among %d agents: each needs at least one", j.Workers, n)
	case len(j.Offsets) > 0 && len(j.Offsets) < n:
		return nil, fmt.Errorf("a trace of %d requests cannot be shared among %d agents: each needs at least one", len(j.Offsets), n)
	case len(j.Offsets) == 0 && len(j.Stages) == 0:
		return nil, errors.New("the job has neither a profile nor a trace to share")
	}
	arrivals, err := j.arrivals()
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, n)
	for i := range jobs {
		s := j
		s.Workers = j.Workers / n
		if i < j.Workers%n {
			s.Workers++
		}
		s.Requests = stride(j.Requests, n, i)
		if len(j.Offsets) > 0 {
			s.Offsets = nil
			for k := i; k < len(j.Offsets); k += n {
				s.Offsets = append(s.Offsets, j.Offsets[k])
			}
			s.Span = max(j.Span, j.Offsets[len(j.Offsets)-1])
		} else {
			s.Stages = make([]pacer.Stage, len(j.Stages))
			for k, st := range j.Stages {
				s.Stages[k] = pacer.Stage{Duration: st.Duration, From: st.From / float64(n), To: st.To / float64(n)}
			}
			if arrivals == pacer.Poisson {
				s.Seed = j.Seed + uint64(i)
			} else {
				s.Phase = (j.Phase + float64(i)) / float64(n)
			}
		}
		jobs[i] = s
	}
	return jobs, nil
}

// stride returns what agent i of n sends, cycled, so that its k-th request is
// the whole test's (n*k+i)-th: reqs[(n*k+i) mod len(reqs)]. That repeats after
// len/gcd(n, len) requests, so the list is that long: every n-th request when
// n divides the trace, and all of them in another order when it does not.
func stride(reqs []runner.Request, n, i int) []runner.Request {
	if len(reqs) == 0 {
		return nil
	}
	period := len(reqs) / gcd(n, len(reqs))
	out := make([]runner.Request, period)
	for k := range out {
		out[k] = reqs[(n*k+i)%len(reqs)]
	}
	return out
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
