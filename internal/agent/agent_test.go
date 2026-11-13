package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

const token = "a-test-password-of-some-length"

func target(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv
}

// start serves s over HTTP and returns a client of it with the right password.
func start(t *testing.T, s *Server) *Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return NewClient(srv.Listener.Addr().String(), token)
}

func newServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(token, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// job is 30 requests over 300ms.
func job(url string) Job {
	return Job{
		URL:         url,
		Method:      http.MethodGet,
		Stages:      pacer.ConstantRate(100, 300*time.Millisecond),
		Workers:     4,
		Timeout:     2 * time.Second,
		SampleEvery: 100 * time.Millisecond,
	}
}

// fakeRun stands in for the runner: it counts its calls and returns an empty
// result that can be merged.
func fakeRun(calls *atomic.Int32) func(context.Context, runner.Config) (*runner.Result, error) {
	return func(context.Context, runner.Config) (*runner.Result, error) {
		calls.Add(1)
		return &runner.Result{Corrected: hist.New(), Service: hist.New(), TimerLag: hist.New()}, nil
	}
}

func TestAnAgentRunsTheJobAndSendsBackAResultThatCanBeMerged(t *testing.T) {
	c := start(t, newServer(t))
	res, err := c.Run(context.Background(), job(target(t).URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Scheduled != 30 || res.Statuses[200] != 30 || res.Corrected.Count() != 30 {
		t.Errorf("scheduled %d, statuses %v, %d latencies; want 30 OK", res.Scheduled, res.Statuses, res.Corrected.Count())
	}
	if len(res.Timeline) < 3 {
		t.Fatalf("%d windows of 100ms in a 300ms run", len(res.Timeline))
	}
	for i, w := range res.Timeline {
		if w.Latency == nil {
			t.Errorf("window %d came back without its distribution, so it cannot be merged", i+1)
		}
	}
	if _, err := runner.Merge(res, res); err != nil {
		t.Errorf("the result cannot be merged: %v", err)
	}
}

func TestAnAgentWantsALongPassword(t *testing.T) {
	for _, pw := range []string{"", "short", strings.Repeat("x", MinTokenLength-1)} {
		if _, err := NewServer(pw, t.Logf); err == nil {
			t.Errorf("accepted the password %q", pw)
		}
	}
	if _, err := NewServer(strings.Repeat("x", MinTokenLength), t.Logf); err != nil {
		t.Errorf("refused a password of %d characters: %v", MinTokenLength, err)
	}
}

func TestAnAgentRunsNothingWithoutThePassword(t *testing.T) {
	s := newServer(t)
	var calls atomic.Int32
	s.run = fakeRun(&calls)
	srv := httptest.NewServer(s)
	defer srv.Close()

	for _, pw := range []string{"", "a-test-password-of-some-lengtH", token + "x"} {
		c := NewClient(srv.URL, pw)
		if _, err := c.Run(context.Background(), job(target(t).URL)); err == nil || !strings.Contains(err.Error(), "password") {
			t.Errorf("password %q: Run said %v, want a refusal that names the password", pw, err)
		}
		if _, err := c.Ping(context.Background()); err == nil {
			t.Errorf("password %q: Ping succeeded", pw)
		}
	}
	for _, header := range []string{token, "Basic " + token, "bearer " + token} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/ping", nil)
		req.Header.Set("Authorization", header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: %s, want 401: the password goes as a Bearer token", strings.ReplaceAll(header, token, "<password>"), resp.Status)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("ran %d jobs for callers without the password", calls.Load())
	}
}

func TestAnAgentRunsOneJobAtATime(t *testing.T) {
	s := newServer(t)
	release := make(chan struct{})
	s.run = func(context.Context, runner.Config) (*runner.Result, error) {
		<-release
		return &runner.Result{Corrected: hist.New(), Service: hist.New(), TimerLag: hist.New()}, nil
	}
	c := start(t, s)
	first := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), job(target(t).URL))
		first <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := c.Ping(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never said it was busy")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Run(ctx, job(target(t).URL)); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("a second job while the first ran: %v, want a refusal saying the agent is busy", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Errorf("the first job: %v", err)
	}
	if st, err := c.Ping(context.Background()); err != nil || st.Busy || st.Version != Version {
		t.Errorf("after the run: %+v, %v; want an idle agent of version %d", st, err, Version)
	}
}

func TestAnAgentRefusesAJobItCannotRunBeforeWaitingToStart(t *testing.T) {
	s := newServer(t)
	var calls atomic.Int32
	s.run = fakeRun(&calls)
	c := start(t, s)
	url := target(t).URL
	bad := map[string]func(*Job){
		"no workers":          func(j *Job) { j.Workers = 0 },
		"unknown arrivals":    func(j *Job) { j.Arrivals = "bursty" },
		"insecure and cacert": func(j *Job) { j.Insecure, j.CACert = true, []byte("x") },
		"cacert not PEM":      func(j *Job) { j.CACert = []byte("not a certificate") },
		"start far off":       func(j *Job) { j.StartIn = MaxStartIn + time.Second },
		"start in the past":   func(j *Job) { j.StartIn = -time.Second },
	}
	for name, spoil := range bad {
		j := job(url)
		j.StartIn = 5 * time.Second
		spoil(&j)
		began := time.Now()
		_, err := c.Run(context.Background(), j)
		if err == nil {
			t.Errorf("%s: the agent took the job", name)
		}
		if waited := time.Since(began); waited > time.Second {
			t.Errorf("%s: refused only after %v, not before waiting to start", name, waited)
		}
	}
	// The client always sends its own version, so an agent of another build is
	// what sends another: send one by hand.
	j := job(url)
	j.Version = Version + 1
	raw, _ := json.Marshal(j)
	req, _ := http.NewRequest(http.MethodPost, c.base+"/v1/run", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a job of another version: %s, want 400 Bad Request", resp.Status)
	}
	if calls.Load() != 0 {
		t.Errorf("ran %d bad jobs", calls.Load())
	}
}

func TestAnAgentRefusesAnOversizedJob(t *testing.T) {
	old := maxJobBytes
	maxJobBytes = 1000
	t.Cleanup(func() { maxJobBytes = old })
	s := newServer(t)
	var calls atomic.Int32
	s.run = fakeRun(&calls)
	j := job(target(t).URL)
	j.Body = make([]byte, 2000)
	_, err := start(t, s).Run(context.Background(), j)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("Run = %v, want a refusal of a job too large", err)
	}
	if calls.Load() != 0 {
		t.Error("ran a job it should have refused")
	}
}

func TestAnAgentStartsWhenItIsToldTo(t *testing.T) {
	s := newServer(t)
	var ran time.Time
	s.run = func(context.Context, runner.Config) (*runner.Result, error) {
		ran = time.Now()
		return &runner.Result{Corrected: hist.New(), Service: hist.New(), TimerLag: hist.New()}, nil
	}
	j := job(target(t).URL)
	j.StartIn = 300 * time.Millisecond
	sent := time.Now()
	if _, err := start(t, s).Run(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if waited := ran.Sub(sent); waited < 300*time.Millisecond || waited > 2*time.Second {
		t.Errorf("started %v after the job was sent, want 300ms", waited)
	}
}

func TestAnAgentDropsAJobWhoseControllerWentAwayBeforeItStarted(t *testing.T) {
	s := newServer(t)
	var calls atomic.Int32
	s.run = fakeRun(&calls)
	c := start(t, s)
	j := job(target(t).URL)
	j.StartIn = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Run(ctx, j); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v, want the deadline", err)
	}
	time.Sleep(time.Second) // past when it would have started
	if calls.Load() != 0 {
		t.Error("the agent ran a job nobody was waiting for")
	}
	if st, err := c.Ping(context.Background()); err != nil || st.Busy {
		t.Errorf("after it: %+v, %v, want an idle agent", st, err)
	}
}

func TestAnAgentStopsWhenTheControllerGoesAway(t *testing.T) {
	s := newServer(t)
	running, stopped := make(chan struct{}), make(chan struct{})
	s.run = func(ctx context.Context, _ runner.Config) (*runner.Result, error) {
		close(running)
		<-ctx.Done()
		close(stopped)
		return &runner.Result{Corrected: hist.New(), Service: hist.New(), TimerLag: hist.New()}, nil
	}
	c := start(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Run(ctx, job(target(t).URL))
		done <- err
	}()
	<-running
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the run went on after the controller went away")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want the cancellation", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := c.Ping(context.Background())
		if err == nil && !st.Busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent stayed busy: %+v, %v", st, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A job sent by something other than Client (curl, or json.Encoder, which ends
// it with a newline) may have more after the JSON; the agent must still notice
// when its sender goes away.
func TestAnAgentNoticesTheControllerGoingAwayWhateverFollowsTheJob(t *testing.T) {
	s := newServer(t)
	running, stopped := make(chan struct{}), make(chan bool, 1)
	s.run = func(ctx context.Context, _ runner.Config) (*runner.Result, error) {
		close(running)
		select {
		case <-ctx.Done():
			stopped <- true
		case <-time.After(5 * time.Second):
			stopped <- false
		}
		return nil, ctx.Err()
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	j := job(target(t).URL)
	j.Version = Version
	raw, _ := json.Marshal(j)
	body := string(raw) + strings.Repeat("\n", 100_000)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/run", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	go func() {
		<-running
		cancel()
	}()
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	if !<-stopped {
		t.Error("the run went on after its controller went away")
	}
}

func TestTheClientRefusesAResultItCouldNotMerge(t *testing.T) {
	good := func() *runner.Result {
		return &runner.Result{
			Corrected: hist.New(), Service: hist.New(), TimerLag: hist.New(),
			Timeline: []runner.Window{{Latency: hist.New()}},
			Stages:   []runner.StageResult{{Corrected: hist.New(), Service: hist.New()}},
			Labels:   []runner.LabelResult{{Label: "x", Corrected: hist.New(), Service: hist.New()}},
			Phases:   []runner.PhaseResult{{Phase: "wait", Samples: hist.New()}},
		}
	}
	bad := map[string]func(*runner.Result){
		"none":                    nil,
		"no corrected latency":    func(r *runner.Result) { r.Corrected = nil },
		"no service latency":      func(r *runner.Result) { r.Service = nil },
		"no timer lag":            func(r *runner.Result) { r.TimerLag = nil },
		"a window without one":    func(r *runner.Result) { r.Timeline[0].Latency = nil },
		"a stage without one":     func(r *runner.Result) { r.Stages[0].Corrected = nil },
		"a stage without service": func(r *runner.Result) { r.Stages[0].Service = nil },
		"a label without one":     func(r *runner.Result) { r.Labels[0].Corrected = nil },
		"a label without service": func(r *runner.Result) { r.Labels[0].Service = nil },
		"a phase without samples": func(r *runner.Result) { r.Phases[0].Samples = nil },
	}
	check := func(r *runner.Result) error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(r)
		}))
		defer srv.Close()
		_, err := NewClient(srv.URL, token).Run(context.Background(), job("http://x/"))
		return err
	}
	if err := check(good()); err != nil {
		t.Fatalf("a good result: %v", err)
	}
	for name, spoil := range bad {
		var r *runner.Result
		if spoil != nil {
			r = good()
			spoil(r)
		}
		if err := check(r); err == nil {
			t.Errorf("%s: the client took it", name)
		}
	}
}

func TestAJobCarriesEverythingARunNeeds(t *testing.T) {
	j := Job{
		Version:     Version,
		URL:         "https://example.test/base",
		Method:      http.MethodPost,
		Header:      http.Header{"X-A": {"1"}},
		Body:        []byte("hello"),
		Stages:      []pacer.Stage{{Duration: time.Second, From: 10, To: 20}},
		Arrivals:    "poisson",
		Seed:        42,
		Requests:    []runner.Request{{Method: "PUT", Target: "/x", Header: http.Header{"X-B": {"2"}}, Body: []byte("b"), Logged: 503, Label: "PUT /x"}},
		Workers:     7,
		Timeout:     3 * time.Second,
		Spin:        time.Millisecond,
		Phases:      true,
		HTTP2:       true,
		SampleEvery: 250 * time.Millisecond,
	}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var back Job
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	cfg, err := back.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != j.URL || cfg.Method != "POST" || cfg.Header.Get("X-A") != "1" || string(cfg.Body) != "hello" ||
		len(cfg.Stages) != 1 || cfg.Stages[0] != j.Stages[0] || cfg.Arrivals != pacer.Poisson || cfg.Seed != 42 ||
		len(cfg.Requests) != 1 || cfg.Requests[0].Label != "PUT /x" || cfg.Requests[0].Logged != 503 || cfg.Requests[0].Header.Get("X-B") != "2" ||
		cfg.Workers != 7 || cfg.Timeout != 3*time.Second || cfg.Spin != time.Millisecond || !cfg.Phases || !cfg.HTTP2 ||
		cfg.SampleEvery != 250*time.Millisecond || !cfg.WindowHistograms || cfg.TLS != nil {
		t.Errorf("config = %+v", cfg)
	}

	replay := Job{Version: Version, URL: "http://x", Offsets: []time.Duration{0, time.Second}, Span: 2 * time.Second, Workers: 1}
	if cfg, err := roundTrip(t, replay).Config(); err != nil || len(cfg.Offsets) != 2 || cfg.Offsets[1] != time.Second || cfg.Span != 2*time.Second || cfg.Arrivals != pacer.Even {
		t.Errorf("replay config = %+v, %v", cfg, err)
	}

	even := Job{Version: Version, URL: "http://x", Stages: pacer.ConstantRate(10, time.Second), Phase: 0.5, Workers: 1}
	if cfg, err := roundTrip(t, even).Config(); err != nil || cfg.Phase != 0.5 || cfg.Arrivals != pacer.Even {
		t.Errorf("even config = %+v, %v", cfg, err)
	}

	insecure := Job{Version: Version, Insecure: true}
	if cfg, err := insecure.Config(); err != nil || cfg.TLS == nil || !cfg.TLS.InsecureSkipVerify {
		t.Errorf("-insecure became %+v, %v", cfg.TLS, err)
	}
}

func TestAJobTrustsTheCertificatesItCarries(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the failed handshakes are the point
	srv.StartTLS()
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c := start(t, newServer(t))

	j := job(srv.URL)
	j.CACert = ca
	res, err := c.Run(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if res.Statuses[200] != 30 {
		t.Errorf("with the server's certificate: statuses %v errors %v, want 30 OK", res.Statuses, res.Errors)
	}

	res, err = c.Run(context.Background(), job(srv.URL)) // the system's roots do not know the test server
	if err != nil {
		t.Fatal(err)
	}
	if res.Statuses[200] != 0 {
		t.Errorf("without it: %v OK, want every request to fail verification", res.Statuses)
	}
}

// roundTrip is j after being sent as JSON.
func roundTrip(t *testing.T, j Job) Job {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var back Job
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	return back
}
