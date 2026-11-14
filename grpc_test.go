package gmeter_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/optimm/Gmeter"
	"github.com/optimm/Gmeter/internal/grpcdemo"
)

func echo(t *testing.T, r grpcdemo.Reflection, opts ...grpc.ServerOption) (string, *grpcdemo.Echo) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(opts...)
	e := grpcdemo.Register(s, r)
	go s.Serve(ln)
	t.Cleanup(s.Stop)
	return ln.Addr().String(), e
}

func sayConfig(addr string) gmeter.Config {
	return gmeter.Config{
		URL: addr, Rate: 200, Duration: 500 * time.Millisecond,
		GRPC: &gmeter.GRPC{Method: grpcdemo.Say, Plaintext: true, Data: []byte(`{"name":"ann"}`)},
	}
}

func TestRunCallsAGRPCMethodAtTheRate(t *testing.T) {
	addr, e := echo(t, grpcdemo.ReflectionBoth)

	res, err := gmeter.Run(context.Background(), sayConfig(addr))
	if err != nil {
		t.Fatal(err)
	}

	if res.Scheduled() != 100 || res.Responses() != 100 || res.OK() != 100 || res.BadStatus() != 0 || res.Failed() != 0 {
		t.Errorf("scheduled %d, responses %d, ok %d, bad %d, failed %d; want 100, 100, 100, 0, 0",
			res.Scheduled(), res.Responses(), res.OK(), res.BadStatus(), res.Failed())
	}
	if res.Codes()["OK"] != 100 || len(res.Statuses()) != 0 {
		t.Errorf("codes %v, statuses %v; want 100 OK and no HTTP statuses", res.Codes(), res.Statuses())
	}
	if e.Names()["ann"] != 100 {
		t.Errorf("the server saw %v, want 100 calls with the request's name", e.Names())
	}
	if res.Corrected(0.99) <= 0 {
		t.Error("no latency recorded")
	}
	var text strings.Builder
	if err := res.WriteText(&text); err != nil || !strings.Contains(text.String(), "OK: 100") {
		t.Errorf("report (err %v) has no gRPC codes line:\n%s", err, text.String())
	}
}

func TestAGRPCStatusThatIsNotOKIsABadStatus(t *testing.T) {
	addr, _ := echo(t, grpcdemo.ReflectionBoth)
	cfg := sayConfig(addr)
	cfg.GRPC.Data = []byte(`{"code":"NOT_FOUND"}`)

	res, err := gmeter.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	if res.Codes()["NOT_FOUND"] != 100 || res.OK() != 0 || res.BadStatus() != 100 || res.ErrorRate() != 1 {
		t.Errorf("codes %v, ok %d, bad %d, error rate %v; want 100 NOT_FOUND all bad", res.Codes(), res.OK(), res.BadStatus(), res.ErrorRate())
	}
	err = res.Meets(gmeter.SLO{Quantile: 0.99, Latency: time.Second})
	if !errors.Is(err, gmeter.ErrTargetMissed) || !strings.Contains(err.Error(), "error") {
		t.Errorf("Meets = %v, want a missed target that blames the errors", err)
	}
}

func TestAGRPCRunSendsTheMetadataOverTheConnectionsAndNeedsNoReflectionWithAProtoset(t *testing.T) {
	addr, e := echo(t, grpcdemo.NoReflection)
	_, protoset := grpcdemo.Files()
	cfg := sayConfig(addr)
	cfg.GRPC.Protoset = protoset
	cfg.GRPC.Conns = 2
	cfg.GRPC.Metadata = map[string][]string{"X-Team": {"payments"}}

	res, err := gmeter.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	if res.OK() != 100 {
		t.Errorf("%d ok, want 100", res.OK())
	}
	if got := e.Metadata().Get("x-team"); len(got) != 1 || got[0] != "payments" {
		t.Errorf("the server saw x-team %v, want [payments]", got)
	}
	if e.Peers() != 2 {
		t.Errorf("calls came over %d connections, want 2", e.Peers())
	}
}

func TestOnRequestSeesTheStatusCodeOfACall(t *testing.T) {
	addr, _ := echo(t, grpcdemo.ReflectionBoth)
	cfg := sayConfig(addr)
	cfg.GRPC.Data = []byte(`{"code":"NOT_FOUND"}`)
	var mu sync.Mutex
	var got []gmeter.Sample
	cfg.OnRequest = func(s gmeter.Sample) { mu.Lock(); got = append(got, s); mu.Unlock() }

	if _, err := gmeter.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	if len(got) != 100 || got[0].Code != "NOT_FOUND" || got[0].Status != 0 || got[0].Failure != "" {
		t.Errorf("%d samples, first %+v; want 100 with Code NOT_FOUND", len(got), got[0])
	}
}

func TestAGRPCConfigThatCannotRunIsRefused(t *testing.T) {
	addr, _ := echo(t, grpcdemo.ReflectionBoth)
	with := func(change func(*gmeter.Config)) gmeter.Config {
		c := sayConfig(addr)
		change(&c)
		return c
	}
	for name, tc := range map[string]struct {
		cfg  gmeter.Config
		want string
	}{
		"no method":               {with(func(c *gmeter.Config) { c.GRPC.Method = "" }), "Method"},
		"unknown method":          {with(func(c *gmeter.Config) { c.GRPC.Method = "gmeter.demo.Echo/Shout" }), "has no method Shout"},
		"a stream":                {with(func(c *gmeter.Config) { c.GRPC.Method = "gmeter.demo.Echo/Chat" }), "streaming"},
		"a misspelt field":        {with(func(c *gmeter.Config) { c.GRPC.Data = []byte(`{"nme":"x"}`) }), "unknown field"},
		"HTTP method":             {with(func(c *gmeter.Config) { c.Method = "POST" }), "HTTP"},
		"HTTP header":             {with(func(c *gmeter.Config) { c.Header = http.Header{"A": {"b"}} }), "HTTP"},
		"HTTP body":               {with(func(c *gmeter.Config) { c.Body = []byte("x") }), "HTTP"},
		"HTTP requests":           {with(func(c *gmeter.Config) { c.Requests = []gmeter.Request{{Path: "/"}} }), "HTTP"},
		"HTTP/2":                  {with(func(c *gmeter.Config) { c.HTTP2 = true }), "HTTP"},
		"plaintext TLS":           {with(func(c *gmeter.Config) { c.TLS = &tls.Config{} }), "plaintext"},
		"a URL":                   {with(func(c *gmeter.Config) { c.URL = "http://" + addr }), "host:port"},
		"negative conns":          {with(func(c *gmeter.Config) { c.GRPC.Conns = -1 }), "Conns"},
		"more conns than workers": {with(func(c *gmeter.Config) { c.GRPC.Conns, c.Workers = 5, 2 }), "Conns"},
		"nothing listening":       {with(func(c *gmeter.Config) { c.URL = "127.0.0.1:1" }), "cannot connect"},
	} {
		if _, err := gmeter.Run(context.Background(), tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "gmeter: ") {
			t.Errorf("%s: error %v, want one starting with \"gmeter: \" and mentioning %q", name, err, tc.want)
		}
	}
}

func TestAGRPCRunOverTLSTrustsTheConfiguredRoots(t *testing.T) {
	https := httptest.NewUnstartedServer(nil) // only for its certificate
	https.StartTLS()
	t.Cleanup(https.Close)
	addr, _ := echo(t, grpcdemo.ReflectionBoth, grpc.Creds(credentials.NewServerTLSFromCert(&https.TLS.Certificates[0])))
	cfg := sayConfig(addr)
	cfg.GRPC.Plaintext = false

	if _, err := gmeter.Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("without the roots: error %v, want a certificate error", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(https.Certificate())
	cfg.TLS = &tls.Config{RootCAs: pool}
	res, err := gmeter.Run(context.Background(), cfg)
	if err != nil || res.OK() != 100 {
		t.Errorf("with the roots: error %v, %v ok, want 100 ok", err, res)
	}
}

func TestFindCapacityOfAGRPCMethod(t *testing.T) {
	addr, _ := echo(t, grpcdemo.ReflectionBoth)
	cfg := sayConfig(addr)
	cfg.Rate, cfg.Duration = 0, 0

	got, err := gmeter.FindCapacity(context.Background(), cfg, gmeter.SLO{Quantile: 0.99, Latency: time.Second},
		gmeter.SearchOptions{Start: 20, Max: 40, Trial: 300 * time.Millisecond, Warmup: -1, Confirm: -1})
	if err != nil {
		t.Fatal(err)
	}

	if !got.Found || got.MaxRPS < 20 || got.MaxRPS > 40 {
		t.Errorf("found %v at %v rps, want a rate from 20 to 40 (40, the most it was allowed, on a quiet machine)", got.Found, got.MaxRPS)
	}
}
