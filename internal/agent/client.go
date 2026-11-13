package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/optimm/Gmeter/internal/runner"
)

// Client talks to one agent.
type Client struct {
	base  string // http://host:port
	token string
	http  *http.Client
}

// NewClient returns a client of the agent at addr (host:port, or a URL),
// which must know the agent's password, token.
func NewClient(addr, token string) *Client {
	base := addr
	if !strings.Contains(addr, "://") {
		base = "http://" + addr
	}
	// No overall timeout: a run takes as long as it takes, and ctx ends it.
	return &Client{base: strings.TrimSuffix(base, "/"), token: token, http: &http.Client{}}
}

// Ping asks the agent how it is.
func (c *Client) Ping(ctx context.Context) (Status, error) {
	var st Status
	resp, err := c.do(ctx, http.MethodGet, "/v1/ping", nil)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return st, fmt.Errorf("agent %s: cannot read its status: %w", c.base, err)
	}
	return st, nil
}

// Run sends the agent job (of this Version, whatever job.Version says) and
// waits for the result, which it checks can be merged. Cancelling ctx stops
// the run on the agent.
func (c *Client) Run(ctx context.Context, job Job) (*runner.Result, error) {
	job.Version = Version
	body, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/run", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res *runner.Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("agent %s: cannot read its result: %w", c.base, err)
	}
	if err := mergeable(res); err != nil {
		return nil, fmt.Errorf("agent %s: %w", c.base, err)
	}
	return res, nil
}

// do sends a request and returns the response if it is a 200.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("agent %s: %w", c.base, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("agent %s: %s: %s", c.base, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// mergeable reports what a result lacks that runner.Merge needs, so that an
// agent of another build fails here and not with a crash while merging.
func mergeable(r *runner.Result) error {
	if r == nil {
		return errors.New("it sent no result")
	}
	if r.Corrected == nil || r.Service == nil || r.TimerLag == nil {
		return errors.New("its result has no latency distribution")
	}
	for i, w := range r.Timeline {
		if w.Latency == nil {
			return fmt.Errorf("window %d of its result has no latency distribution", i+1)
		}
	}
	for i, st := range r.Stages {
		if st.Corrected == nil || st.Service == nil {
			return fmt.Errorf("stage %d of its result has no latency distribution", i+1)
		}
	}
	for _, l := range r.Labels {
		if l.Corrected == nil || l.Service == nil {
			return fmt.Errorf("endpoint %q of its result has no latency distribution", l.Label)
		}
	}
	for _, p := range r.Phases {
		if p.Samples == nil {
			return fmt.Errorf("phase %s of its result has no distribution", p.Phase)
		}
	}
	return nil
}
