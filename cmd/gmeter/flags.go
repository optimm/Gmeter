package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
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
	insecure *bool
	cacert   *string
	http2    *bool
	headers  headerFlag
	phases   *bool // nil for a command that does not offer -phases
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
		insecure: fs.Bool("insecure", false, "do not verify the server's TLS certificate (https only); the run then measures a server it has not checked"),
		cacert:   fs.String("cacert", "", "PEM file with the certificate authorities to trust for https, instead of the system's (https only)"),
		http2:    fs.Bool("http2", false, "send over HTTP/2 only (negotiated over TLS for https, h2c with prior knowledge for http); a server without it fails the requests instead of being measured over HTTP/1.1"),
	}
	fs.Var(&r.headers, "H", `extra request header "Key: Value" (repeatable)`)
	return r
}

// addPhasesFlag registers -phases, for the commands that report where a
// request's time went: a run and a replay, and a capacity search, which says
// where the time grew between the answer and the first rate that failed.
func addPhasesFlag(fs *flag.FlagSet, r *requestFlags) {
	r.phases = fs.Bool("phases", true, "split each request's latency into phases (queue, dns, connect, tls, send, wait, transfer); costs about 16 small allocations per request, -phases=false turns it off")
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
	tlsConfig, err := r.tlsConfig()
	if err != nil {
		return runner.Config{}, err
	}
	return runner.Config{
		URL:         url,
		Method:      method,
		Header:      http.Header(r.headers),
		Body:        payload,
		Workers:     *r.workers,
		Timeout:     *r.timeout,
		Spin:        *r.spin,
		Phases:      r.phases != nil && *r.phases,
		HTTP2:       *r.http2,
		TLS:         tlsConfig,
		SampleEvery: sampleEvery, // the same windows whether or not progress is shown
	}, nil
}

// tlsConfig builds the client TLS configuration the flags ask for, or nil for
// the default: verify against the system's roots.
func (r *requestFlags) tlsConfig() (*tls.Config, error) {
	switch {
	case *r.insecure && *r.cacert != "":
		return nil, errors.New("-insecure and -cacert cannot be combined: -insecure trusts every certificate, so there is nothing for -cacert to add")
	case *r.insecure:
		return &tls.Config{InsecureSkipVerify: true}, nil // the user asked for it by name
	case *r.cacert != "":
		data, err := os.ReadFile(*r.cacert)
		if err != nil {
			return nil, fmt.Errorf("-cacert: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("-cacert: no certificate found in %s (it must be PEM)", *r.cacert)
		}
		return &tls.Config{RootCAs: roots}, nil
	}
	return nil, nil
}

// protocol is the protocol asked for, as saved in a run's JSON: "HTTP/2", or
// empty for the default, HTTP/1.1.
func (r *requestFlags) protocol() string {
	if *r.http2 {
		return "HTTP/2"
	}
	return ""
}

// protocolText says the protocol in a banner, if it is not the default.
func (r *requestFlags) protocolText() string {
	if *r.http2 {
		return ", over HTTP/2"
	}
	return ""
}

// noteTLS says on stderr that the server is not being verified, so a result
// from a server nobody checked is not taken for one that was.
func (r *requestFlags) noteTLS(stderr io.Writer) {
	if *r.insecure && !*r.quiet {
		fmt.Fprintln(stderr, "gmeter: -insecure: the server's certificate is not verified")
	}
}
