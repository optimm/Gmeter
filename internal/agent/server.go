package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// Server is an agent: an http.Handler that runs the jobs it is sent, one at a
// time, and answers each with its runner.Result as JSON.
//
//	GET  /v1/ping  the agent's Status
//	POST /v1/run   a Job; the answer comes when the run is over
//
// Both need the header "Authorization: Bearer <password>". A run stops if the
// caller goes away, so a controller that gives up does not leave load behind.
type Server struct {
	token [sha256.Size]byte // compared as hashes, so the comparison takes as long whatever the length
	logf  func(format string, args ...any)
	busy  atomic.Bool
	run   func(context.Context, runner.Config) (*runner.Result, error) // runner.Run; tests replace it
	mux   *http.ServeMux
}

// NewServer returns an agent that takes jobs from callers that know token,
// which must be at least MinTokenLength characters. It logs each job with logf.
func NewServer(token string, logf func(format string, args ...any)) (*Server, error) {
	if len(token) < MinTokenLength {
		return nil, fmt.Errorf("the password must be at least %d characters (it can be guessed at over the network); `openssl rand -hex 16` makes one", MinTokenLength)
	}
	s := &Server{token: sha256.Sum256([]byte(token)), logf: logf, run: runner.Run, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v1/ping", s.ping)
	s.mux.HandleFunc("POST /v1/run", s.runJob)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	hash := sha256.Sum256([]byte(given))
	if !ok || subtle.ConstantTimeCompare(hash[:], s.token[:]) != 1 {
		s.logf("refused %s %s from %s: wrong or missing password", r.Method, r.URL.Path, r.RemoteAddr)
		http.Error(w, "wrong or missing password", http.StatusUnauthorized)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) ping(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, Status{Version: Version, Busy: s.busy.Load()})
}

func (s *Server) runJob(w http.ResponseWriter, r *http.Request) {
	if !s.busy.CompareAndSwap(false, true) {
		http.Error(w, "busy: this agent is already running a job", http.StatusConflict)
		return
	}
	defer s.busy.Store(false)

	var job Job
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJobBytes)).Decode(&job)
	if err == nil {
		// Read to the end: the server only notices that the caller went away once
		// the body has been read, and a run must stop when it does.
		_, err = io.Copy(io.Discard, r.Body)
	}
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, fmt.Sprintf("the job is too large: over %d bytes", mbe.Limit), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read the job: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := job.Config()
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.logf("job from %s: %s %s, %d workers, starting in %v", r.RemoteAddr, methodOf(cfg), cfg.URL, cfg.Workers, job.StartIn)
	ctx := r.Context()
	t := time.NewTimer(job.StartIn)
	select {
	case <-t.C:
	case <-ctx.Done():
		t.Stop()
		s.logf("the controller went away before the job started")
		return
	}
	began := time.Now()
	res, err := s.run(ctx, cfg)
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case ctx.Err() != nil:
		s.logf("the controller went away: stopped the job after %v", time.Since(began).Round(time.Millisecond))
	default:
		s.logf("job done in %v: %d requests", time.Since(began).Round(time.Millisecond), res.Scheduled)
		writeJSON(w, res)
	}
}

func methodOf(cfg runner.Config) string {
	if len(cfg.Requests) > 0 {
		return fmt.Sprintf("%d requests of a trace to", len(cfg.Requests))
	}
	if cfg.Method == "" {
		return http.MethodGet
	}
	return cfg.Method
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) // nothing to be done about a failed write: the caller has gone
}
