package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// h2cServer answers over HTTP/1.1, and over HTTP/2 without TLS (h2c) if h2 is set.
func h2cServer(t *testing.T, h2 bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(h2)
	srv.Config.Protocols = &p
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTP2FlagSendsOverHTTP2AndSaysSo(t *testing.T) {
	srv := h2cServer(t, true)
	path := filepath.Join(t.TempDir(), "run.json")
	code, stdout, stderr := exec(t, "-http2", "-rate", "50", "-duration", "200ms", "-quiet", "-json", path, srv.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "protocol   HTTP/2.0: 10") {
		t.Errorf("the report does not say the 10 responses came over HTTP/2:\n%s", stdout)
	}
	raw, _ := os.ReadFile(path)
	var saved struct {
		Run struct {
			Protocol string `json:"protocol"`
		} `json:"run"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Run.Protocol != "HTTP/2" {
		t.Errorf("run.protocol = %q (%v), want HTTP/2", saved.Run.Protocol, err)
	}
}

func TestHTTP2AgainstAnHTTP1ServerIsReportedNotHidden(t *testing.T) {
	srv := h2cServer(t, false)
	code, stdout, stderr := exec(t, "-http2", "-rate", "50", "-duration", "200ms", "-quiet", srv.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// A server that closes the connection on seeing the HTTP/2 preface can make
	// a request see the reset first (Windows does, now and then), so the
	// protocol errors are most of the 10, not always all of them.
	if !strings.Contains(stdout, "10 failed") || !strings.Contains(stdout, "0 ok") ||
		!strings.Contains(stdout, "protocol: ") || !strings.Contains(stdout, "did not speak HTTP/2") {
		t.Errorf("want all 10 failed, mostly as protocol errors, and the warning explaining them:\n%s", stdout)
	}
}

func TestReplayAndFindTakeHTTP2(t *testing.T) {
	srv := h2cServer(t, true)
	trace := writeTrace(t, "trace.jsonl", "{\"path\":\"/a\"}\n{\"path\":\"/b\"}\n")
	path := filepath.Join(t.TempDir(), "replay.json")
	code, stdout, stderr := execReplay(t, "-http2", "-rate", "20", "-arrivals", "even", "-file", trace, "-quiet", "-json", path, srv.URL)
	if code != 0 || !strings.Contains(stdout, "HTTP/2.0: 2") {
		t.Errorf("replay: exit %d, stdout:\n%s\nstderr: %s", code, stdout, stderr)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), `"protocol": "HTTP/2"`) {
		t.Errorf("the replay's JSON does not record HTTP/2:\n%s", raw)
	}
	fast(t)
	code, stdout, stderr = exec(t, findArgs(srv.URL, "-http2", "-start", "50", "-max", "50", "-confirm", "0")...)
	if code != 0 || !strings.Contains(stderr, "HTTP/2") {
		t.Errorf("find: exit %d, want 0 and HTTP/2 named in its banner; stdout:\n%s\nstderr: %s", code, stdout, stderr)
	}
}
