package main

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/optimm/Gmeter/internal/grpcdemo"
)

// echoServer runs the demo gRPC service until the test ends.
func echoServer(t *testing.T, r grpcdemo.Reflection, opts ...grpc.ServerOption) (string, *grpcdemo.Echo) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(opts...)
	echo := grpcdemo.Register(s, r)
	go s.Serve(ln)
	t.Cleanup(s.Stop)
	return ln.Addr().String(), echo
}

// tlsEchoServer serves over TLS with a certificate no system root vouches
// for, and returns the file -cacert needs to trust it.
func tlsEchoServer(t *testing.T) (addr, ca string) {
	t.Helper()
	https := httptest.NewUnstartedServer(nil) // only for its certificate
	https.StartTLS()
	t.Cleanup(https.Close)
	addr, _ = echoServer(t, grpcdemo.ReflectionBoth, grpc.Creds(credentials.NewServerTLSFromCert(&https.TLS.Certificates[0])))
	return addr, certFile(t, https)
}

type grpcRun struct {
	Run struct {
		URL      string  `json:"url"`
		Method   string  `json:"method"`
		Workers  int     `json:"workers"`
		Profile  string  `json:"profile"`
		Protocol string  `json:"protocol"`
		Arrivals string  `json:"arrivals"`
		Seed     uint64  `json:"seed"`
		Timeout  float64 `json:"timeout_seconds"`
		Spin     float64 `json:"spin_seconds"`
	} `json:"run"`
	Scheduled uint64            `json:"scheduled"`
	OK        uint64            `json:"ok"`
	BadStatus uint64            `json:"bad_status"`
	Codes     map[string]uint64 `json:"codes"`
	Statuses  map[string]uint64 `json:"statuses"`
	Timeline  []struct{}        `json:"timeline"`
}

func grpcSaved(t *testing.T, args ...string) (grpcRun, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.json")
	code, stdout, stderr := exec(t, append([]string{"grpc", "-json", path}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r grpcRun
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r, stdout, stderr
}

func TestGRPCCallsAMethodAtARate(t *testing.T) {
	addr, echo := echoServer(t, grpcdemo.ReflectionBoth)
	run, stdout, stderr := grpcSaved(t, "-plaintext", "-call", "gmeter.demo.Echo/Say", "-data", `{"name":"ann"}`,
		"-rate", "100", "-duration", "300ms", "-workers", "4", "-H", "X-Team: load", "-conns", "2", "-spin", "1ms", addr)

	if echo.Names()["ann"] != 30 {
		t.Errorf("the server got %v, want 30 calls for ann", echo.Names())
	}
	if got := echo.Metadata().Get("x-team"); len(got) != 1 || got[0] != "load" {
		t.Errorf("metadata x-team = %v, want load", got)
	}
	if echo.Peers() != 2 {
		t.Errorf("the calls came over %d connections, want 2", echo.Peers())
	}
	if run.Scheduled != 30 || run.OK != 30 || run.Codes["OK"] != 30 || len(run.Statuses) != 0 {
		t.Errorf("saved %+v: want 30 OK codes and no HTTP statuses", run)
	}
	if run.Run.URL != addr || run.Run.Method != "/gmeter.demo.Echo/Say" || run.Run.Protocol != "gRPC" || run.Run.Profile != "300ms@100" || run.Run.Workers != 4 ||
		run.Run.Arrivals != "even" || run.Run.Spin != 0.001 || run.Run.Timeout != 10 {
		t.Errorf("saved run %+v", run.Run)
	}
	if !strings.Contains(stdout, "codes      OK: 30") {
		t.Errorf("no codes line:\n%s", stdout)
	}
	for _, want := range []string{"/gmeter.demo.Echo/Say", addr, "100 rps", "2 connections", "up to 4 in flight"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the banner does not say %q:\n%s", want, stderr)
		}
	}
}

func TestGRPCCountsWhatTheServerAnswered(t *testing.T) {
	addr, _ := echoServer(t, grpcdemo.ReflectionBoth)
	data := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(data, []byte(`{"code":"NOT_FOUND"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run, stdout, _ := grpcSaved(t, "-quiet", "-plaintext", "-call", "gmeter.demo.Echo.Say", "-data", "@"+data, "-rate", "100", "-duration", "200ms", addr)
	if run.Codes["NOT_FOUND"] != 20 || run.BadStatus != 20 || run.OK != 0 {
		t.Errorf("codes %v, bad %d; want 20 NOT_FOUND, all bad", run.Codes, run.BadStatus)
	}
	if !strings.Contains(stdout, "NOT_FOUND: 20") {
		t.Errorf("report:\n%s", stdout)
	}

	run, stdout, _ = grpcSaved(t, "-quiet", "-plaintext", "-call", grpcdemo.Say, "-data", `{"delay_ms":300}`, "-timeout", "50ms", "-rate", "50", "-duration", "200ms", addr)
	if !strings.Contains(stdout, "timeout: 10") || run.Run.Timeout != 0.05 {
		t.Errorf("calls past -timeout are not timeouts:\n%s", stdout)
	}
}

func TestGRPCReadsTheSchemaFromAProtoset(t *testing.T) {
	addr, echo := echoServer(t, grpcdemo.NoReflection)
	_, set := grpcdemo.Files()
	path := filepath.Join(t.TempDir(), "echo.protoset")
	if err := os.WriteFile(path, set, 0o600); err != nil {
		t.Fatal(err)
	}
	run, _, _ := grpcSaved(t, "-quiet", "-plaintext", "-protoset", path, "-call", grpcdemo.Say, "-rate", "50", "-duration", "200ms", "-arrivals", "poisson", "-seed", "5", addr)
	if n := echo.Calls.Load(); n == 0 || uint64(n) != run.OK {
		t.Errorf("%d calls, %d ok; want the same, and some", n, run.OK)
	}
	if run.Run.Arrivals != "poisson" || run.Run.Seed != 5 {
		t.Errorf("saved arrivals %q seed %d, want poisson and 5", run.Run.Arrivals, run.Run.Seed)
	}
}

func TestGRPCOverTLS(t *testing.T) {
	addr, ca := tlsEchoServer(t)
	quick := []string{"-call", grpcdemo.Say, "-rate", "50", "-duration", "200ms"}

	if run, _, _ := grpcSaved(t, append(quick, "-quiet", "-cacert", ca, addr)...); run.OK != 10 {
		t.Errorf("-cacert: %d ok, want 10", run.OK)
	}
	if run, _, stderr := grpcSaved(t, append(quick, "-insecure", addr)...); run.OK != 10 || !strings.Contains(stderr, "not verified") {
		t.Errorf("-insecure: %d ok, want 10, and a note that nothing was verified:\n%s", run.OK, stderr)
	}
	code, stdout, stderr := exec(t, append([]string{"grpc"}, append(quick, addr)...)...)
	if code != 2 || !strings.Contains(stderr, "certificate") || !strings.Contains(stderr, "-cacert") || stdout != "" {
		t.Errorf("an untrusted certificate: exit %d, stderr:\n%s\nwant 2, the reason and what to do", code, stderr)
	}
}

func TestGRPCRefusesWhatItCannotRun(t *testing.T) {
	addr, echo := echoServer(t, grpcdemo.ReflectionBoth)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	call := []string{"-plaintext", "-call", grpcdemo.Say}

	for _, tc := range []struct {
		name string
		args []string
		says string
	}{
		{"no method", []string{"-plaintext", addr}, "-call"},
		{"no address", call, "address"},
		{"two addresses", append(call, addr, addr), "exactly one"},
		{"a URL", append(call, "http://"+addr), "host:port"},
		{"no connections", append(call, "-conns", "0", addr), "-conns"},
		{"more connections than workers", append(call, "-conns", "5", "-workers", "4", addr), "-conns"},
		{"plaintext and TLS settings", append(call, "-insecure", addr), "-plaintext"},
		{"reserved metadata", append(call, "-H", "grpc-timeout: 1S", addr), "grpc-timeout"},
		{"a field the request does not have", append(call, "-data", `{"nmae":"x"}`, addr), "nmae"},
		{"a method the server does not have", []string{"-plaintext", "-call", "gmeter.demo.Echo/Shout", addr}, "Shout"},
		{"a streaming method", []string{"-plaintext", "-call", "gmeter.demo.Echo/Chat", addr}, "stream"},
		{"stages and a rate", append(call, "-stages", "1s@10", "-rate", "5", addr), "-stages"},
		{"a missing protoset", append(call, "-protoset", "/no/such/file", addr), "-protoset"},
		{"nothing listening", []string{"-plaintext", "-call", grpcdemo.Say, closed}, "refused"},
		{"TLS to a server without it", []string{"-call", grpcdemo.Say, addr}, "try -plaintext"},
		{"an HTTP flag", append(call, "-http2", addr), "-http2"},
		{"agents", append(call, "-agents", "127.0.0.1:1", addr), "-agents"},
		{"phases", append(call, "-phases", addr), "-phases"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := exec(t, append([]string{"grpc"}, tc.args...)...)
			if code != 2 || !strings.Contains(stderr, tc.says) || stdout != "" {
				t.Errorf("exit %d, stderr:\n%s\nwant 2 and %q", code, stderr, tc.says)
			}
			if strings.Contains(stderr, "grpc: grpc:") {
				t.Errorf("the prefix is said twice:\n%s", stderr)
			}
		})
	}
	if n := echo.Calls.Load(); n != 0 {
		t.Errorf("%d calls were made", n)
	}
}

func TestGRPCKeepsTimeAsARunDoes(t *testing.T) {
	old := sampleEvery
	sampleEvery = 100 * time.Millisecond
	t.Cleanup(func() { sampleEvery = old })
	addr, _ := echoServer(t, grpcdemo.ReflectionBoth)

	run, _, stderr := grpcSaved(t, "-plaintext", "-call", grpcdemo.Say, "-rate", "100", "-duration", "300ms", addr)
	if len(run.Timeline) < 3 || !strings.Contains(stderr, "window:") {
		t.Errorf("%d windows, progress:\n%s\nwant 100ms windows and a progress line for each", len(run.Timeline), stderr)
	}

	// Poisson arrivals with seed 5 send 103 calls in this profile (even ones 100, seed 6 96).
	for range 2 {
		if run, _, _ := grpcSaved(t, "-quiet", "-plaintext", "-call", grpcdemo.Say, "-rate", "200", "-duration", "500ms", "-arrivals", "poisson", "-seed", "5", addr); run.Scheduled != 103 {
			t.Errorf("%d calls, want the 103 that seed 5 schedules", run.Scheduled)
		}
	}
}

func TestGRPCGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accept, then say nothing
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	start := time.Now()
	code, _, stderr := exec(t, "grpc", "-plaintext", "-call", grpcdemo.Say, "-timeout", "300ms", ln.Addr().String())
	if code != 2 || !strings.Contains(stderr, ln.Addr().String()) || time.Since(start) > 3*time.Second {
		t.Errorf("exit %d after %v, stderr:\n%s\nwant 2 within about -timeout, naming the server", code, time.Since(start), stderr)
	}
}
