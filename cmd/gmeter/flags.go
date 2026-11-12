package main

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

// requestFlags are the flags that say what is sent and how, shared by a plain
// run and by `gmeter find`. Only the load profile differs between them.
type requestFlags struct {
	workers  *int
	timeout  *time.Duration
	spin     *time.Duration
	method   *string
	body     *string
	arrivals *string
	seed     *uint64
	jsonOut  *string
	quiet    *bool
	headers  headerFlag
}

// addRequestFlags registers them on fs. The commands differ in the default
// arrival process: a plain run keeps exact spacing, a capacity search wants
// the bursts a real server sees.
func addRequestFlags(fs *flag.FlagSet, defaultArrivals string, defaultWorkers int) *requestFlags {
	r := addCommonFlags(fs, defaultArrivals, defaultWorkers)
	r.method = fs.String("method", http.MethodGet, "HTTP method")
	r.body = fs.String("body", "", "request body; prefix with @ to read it from a file")
	return r
}

// addCommonFlags registers everything but -method and -body, for a command
// that takes what to send from somewhere else (a replayed trace).
func addCommonFlags(fs *flag.FlagSet, defaultArrivals string, defaultWorkers int) *requestFlags {
	r := &requestFlags{
		workers:  fs.Int("workers", defaultWorkers, "max requests in flight, which is also the max number of connections"),
		timeout:  fs.Duration("timeout", 10*time.Second, "per-request timeout"),
		spin:     fs.Duration("spin", 200*time.Microsecond, "busy-wait this long before each request's due time instead of sleeping, for precise timing (0 = plain timer); capped at a quarter of the gap between requests, so it costs about a quarter of one core at most"),
		arrivals: fs.String("arrivals", defaultArrivals, "how requests are spread within the rate: even (exact spacing) or poisson (random bursts)"),
		seed:     fs.Uint64("seed", 0, "seed for -arrivals poisson, to repeat the same arrival times (0 picks one, and reports it)"),
		jsonOut:  fs.String("json", "", "also write a JSON summary to this path ('-' writes only the JSON, to stdout)"),
		quiet:    fs.Bool("quiet", false, "no progress output on stderr"),
	}
	fs.Var(&r.headers, "H", `extra request header "Key: Value" (repeatable)`)
	return r
}

// arrival parses -arrivals and settles the seed: Poisson with no seed gets a
// random one (never 0, which means "pick one"), and even spacing has none.
func (r *requestFlags) arrival() (pacer.Arrivals, uint64, error) {
	a, err := pacer.ParseArrivals(*r.arrivals)
	if err != nil {
		return 0, 0, fmt.Errorf("-arrivals: %w", err)
	}
	seed := *r.seed
	switch {
	case a != pacer.Poisson:
		seed = 0
	case seed == 0:
		seed = rand.Uint64() | 1
	}
	return a, seed, nil
}

// base is the runner config for url without a load profile.
func (r *requestFlags) base(url string) (runner.Config, error) {
	method, payload := http.MethodGet, []byte(nil)
	if r.method != nil { // a replay has neither flag
		method = strings.ToUpper(*r.method)
		b, err := readBody(*r.body)
		if err != nil {
			return runner.Config{}, fmt.Errorf("cannot read -body: %w", err)
		}
		payload = b
	}
	return runner.Config{
		URL:         url,
		Method:      method,
		Header:      http.Header(r.headers),
		Body:        payload,
		Workers:     *r.workers,
		Timeout:     *r.timeout,
		Spin:        *r.spin,
		SampleEvery: sampleEvery, // the same windows whether or not progress is shown
	}, nil
}
