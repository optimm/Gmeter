package runner

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// protoServer answers every request, after delay, and records the protocol of
// each. With tlsOn it serves https offering HTTP/2 and HTTP/1.1 (or only
// HTTP/1.1 if h2 is false); without, plain http that also accepts HTTP/2 with
// prior knowledge (h2c) if h2 is set.
func protoServer(t *testing.T, tlsOn, h2 bool, delay time.Duration) (*httptest.Server, func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PRI" { // an HTTP/1.1-only server hands HTTP/2's connection preface to the handler
			return
		}
		time.Sleep(delay)
		mu.Lock()
		seen[r.Proto]++
		mu.Unlock()
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	if tlsOn {
		srv.EnableHTTP2 = h2
		srv.StartTLS()
	} else {
		var p http.Protocols
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(h2)
		srv.Config.Protocols = &p
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

func TestHTTP2IsUsedWhenAskedFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tlsOn bool
	}{{"https", true}, {"cleartext (h2c)", false}} {
		srv, seen := protoServer(t, tc.tlsOn, true, 0)
		cfg := baseConfig(srv.URL)
		cfg.HTTP2 = true
		if tc.tlsOn {
			cfg.TLS = &tls.Config{InsecureSkipVerify: true}
		}
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := seen(); got["HTTP/2.0"] != int(res.Scheduled) || len(got) != 1 {
			t.Errorf("%s: the server saw %v, want all %d requests over HTTP/2.0 (errors %v)", tc.name, got, res.Scheduled, res.Errors)
		}
		if res.Protocols["HTTP/2.0"] != res.Scheduled || len(res.Protocols) != 1 {
			t.Errorf("%s: Protocols = %v, want %d HTTP/2.0", tc.name, res.Protocols, res.Scheduled)
		}
	}
}

func TestEveryRunCountsTheProtocolOfItsResponses(t *testing.T) {
	srv, _ := protoServer(t, false, false, 0)
	res, err := Run(context.Background(), baseConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Protocols["HTTP/1.1"] != res.Scheduled || len(res.Protocols) != 1 {
		t.Errorf("Protocols = %v, want %d HTTP/1.1", res.Protocols, res.Scheduled)
	}
}

func TestHTTP2AgainstAServerWithoutItFailsAsProtocolErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tlsOn bool
	}{{"https", true}, {"cleartext", false}} {
		srv, seen := protoServer(t, tc.tlsOn, false, 0)
		cfg := offsetsConfig(srv.URL, 0, 10*time.Millisecond)
		cfg.HTTP2 = true
		if tc.tlsOn {
			cfg.TLS = &tls.Config{InsecureSkipVerify: true}
		}
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := seen(); len(got) != 0 {
			t.Errorf("%s: the server answered %v; HTTP/2 was asked for, so nothing should fall back to HTTP/1.1", tc.name, got)
		}
		if res.Errors["protocol"] != 2 {
			t.Errorf("%s: errors = %v, want 2 protocol errors", tc.name, res.Errors)
		}
	}
}

// HTTP/2 sends many requests over one connection at once: 8 in flight need
// far fewer than 8 connections.
func TestHTTP2MultiplexesRequestsOverFewConnections(t *testing.T) {
	srv, _ := protoServer(t, false, true, 20*time.Millisecond)
	cfg := baseConfig(srv.URL)
	cfg.Rate = 400 // × 20ms = 8 in flight: every worker busy at once
	cfg.HTTP2, cfg.Phases = true, true
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := phaseNamed(t, res, "connect").Samples.Count(); n < 1 || n > 2 {
		t.Errorf("%d requests opened a connection, want 1 (2 if a dial raced): HTTP/2 shares one", n)
	}
	var total time.Duration
	for _, p := range res.Phases {
		total += p.Samples.Sum()
	}
	if total != res.Corrected.Sum() {
		t.Errorf("the phases add up to %v, the corrected latencies to %v", total, res.Corrected.Sum())
	}
	if p50 := phaseNamed(t, res, "wait").Samples.Quantile(0.5); p50 < 19*time.Millisecond {
		t.Errorf("wait p50 = %v, want the server's 20ms", p50)
	}
}
