// Package agent runs a share of a load test on another machine: a controller
// sends each agent a Job over HTTP, every agent runs its job, and the
// controller merges the results it gets back (runner.Merge) into the result of
// the whole test.
//
// An agent sends whatever it is told to wherever it is told, so it only takes
// jobs from a caller that knows its password, and it should listen only on a
// network you trust: neither the password nor the traffic is encrypted.
package agent

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

// Version is the version of the protocol between controller and agent; an
// agent refuses a job of another version, and so a controller and agents from
// different builds that might not agree on what a job means.
const Version = 1

// MinTokenLength is the shortest password an agent accepts: it can be guessed
// at over the network as fast as the agent answers.
const MinTokenLength = 16

// MaxStartIn is the longest an agent will wait to start a job.
const MaxStartIn = time.Minute

// maxJobBytes is the largest job an agent reads, which leaves room for a
// replayed trace of millions of requests; a variable so tests can lower it.
var maxJobBytes int64 = 1 << 30

// Job is one agent's share of a test: a runner.Config that can be sent.
type Job struct {
	Version  int              `json:"version"`
	URL      string           `json:"url"`
	Method   string           `json:"method,omitempty"`
	Header   http.Header      `json:"header,omitempty"`
	Body     []byte           `json:"body,omitempty"`
	Stages   []pacer.Stage    `json:"stages,omitempty"`
	Arrivals string           `json:"arrivals,omitempty"` // "even" (or empty) or "poisson"
	Seed     uint64           `json:"seed,omitempty"`
	Offsets  []time.Duration  `json:"offsets,omitempty"`
	Requests []runner.Request `json:"requests,omitempty"`
	Workers  int              `json:"workers"`
	Timeout  time.Duration    `json:"timeout"`
	Spin     time.Duration    `json:"spin"`
	Phases   bool             `json:"phases,omitempty"`
	HTTP2    bool             `json:"http2,omitempty"`
	// Insecure skips verifying the server's certificate; CACert, PEM, is the
	// certificate authorities to trust instead of the agent's system roots.
	Insecure bool   `json:"insecure,omitempty"`
	CACert   []byte `json:"cacert,omitempty"`
	// SampleEvery is the length of a timeline window, the same on every agent
	// so that their windows line up.
	SampleEvery time.Duration `json:"sample_every"`
	// StartIn is how long after it gets the job the agent starts it. Sent to
	// every agent at once, it starts them together, give or take the time the
	// job takes to reach each.
	StartIn time.Duration `json:"start_in,omitempty"`
}

// Config is the runner configuration of j. It keeps every window's latency
// distribution, which the controller needs to merge the results.
func (j Job) Config() (runner.Config, error) {
	if j.Version != Version {
		return runner.Config{}, fmt.Errorf("the job is of protocol version %d and this agent speaks %d: run the same gmeter build on the controller and the agents", j.Version, Version)
	}
	if j.StartIn < 0 || j.StartIn > MaxStartIn {
		return runner.Config{}, fmt.Errorf("start in %v: want between 0 and %v", j.StartIn, MaxStartIn)
	}
	arrivals := pacer.Even
	if j.Arrivals != "" {
		a, err := pacer.ParseArrivals(j.Arrivals)
		if err != nil {
			return runner.Config{}, err
		}
		arrivals = a
	}
	var tlsConfig *tls.Config
	switch {
	case j.Insecure && len(j.CACert) > 0:
		return runner.Config{}, errors.New("insecure and a CA certificate cannot be combined")
	case j.Insecure:
		tlsConfig = &tls.Config{InsecureSkipVerify: true} // the user asked the controller for it by name
	case len(j.CACert) > 0:
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(j.CACert) {
			return runner.Config{}, errors.New("the CA certificate is not PEM")
		}
		tlsConfig = &tls.Config{RootCAs: roots}
	}
	return runner.Config{
		URL:              j.URL,
		Method:           j.Method,
		Header:           j.Header,
		Body:             j.Body,
		Stages:           j.Stages,
		Arrivals:         arrivals,
		Seed:             j.Seed,
		Offsets:          j.Offsets,
		Requests:         j.Requests,
		Workers:          j.Workers,
		Timeout:          j.Timeout,
		Spin:             j.Spin,
		Phases:           j.Phases,
		HTTP2:            j.HTTP2,
		TLS:              tlsConfig,
		SampleEvery:      j.SampleEvery,
		WindowHistograms: true,
	}, nil
}

// Status is what an agent says about itself.
type Status struct {
	Version int  `json:"version"`
	Busy    bool `json:"busy"` // running a job, so it would refuse another
}
