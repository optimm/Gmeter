package grpcload

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/optimm/Gmeter/internal/grpcdemo"
	"github.com/optimm/Gmeter/internal/runner"
)

// serve runs the demo service until the test ends and returns its address.
func serve(t *testing.T, r grpcdemo.Reflection, opts ...grpc.ServerOption) (string, *grpcdemo.Echo, *grpc.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(opts...)
	echo := grpcdemo.Register(s, r)
	go s.Serve(ln)
	t.Cleanup(s.Stop)
	return ln.Addr().String(), echo, s
}

// listen serves s until the test ends and returns its address.
func listen(t *testing.T, s *grpc.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(s.Stop)
	return ln.Addr().String()
}

func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

// selfSigned is a certificate for 127.0.0.1 that no system root vouches for,
// and a pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gmeter test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func dial(t *testing.T, o Options) (*Caller, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, o)
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func mustDial(t *testing.T, o Options) *Caller {
	t.Helper()
	c, err := dial(t, o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func call(t *testing.T, c *Caller) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Call(ctx)
}

func TestACallFindsItsMethodByServerReflection(t *testing.T) {
	for _, r := range []grpcdemo.Reflection{grpcdemo.ReflectionBoth, grpcdemo.ReflectionV1Alpha} {
		addr, echo, _ := serve(t, r)
		c := mustDial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say, Data: []byte(`{"name":"ann"}`)})
		if code, err := call(t, c); code != "OK" || err != nil {
			t.Errorf("reflection %d: call = %q, %v; want OK", r, code, err)
		}
		if echo.Names()["ann"] != 1 {
			t.Errorf("reflection %d: the server got %v, want one call for ann", r, echo.Names())
		}
		if c.Method() != "/gmeter.demo.Echo/Say" {
			t.Errorf("method %q", c.Method())
		}
		// The name gRPC gives the method can be given back.
		if _, err := dial(t, Options{Addr: addr, Plaintext: true, Method: c.Method()}); err != nil {
			t.Errorf("reflection %d: %s: %v", r, c.Method(), err)
		}
	}
}

func TestACallFindsItsMethodInADescriptorSet(t *testing.T) {
	addr, echo, _ := serve(t, grpcdemo.NoReflection)
	_, set := grpcdemo.Files()
	c := mustDial(t, Options{Addr: addr, Plaintext: true, Method: "gmeter.demo.Echo.Say", Data: []byte(`{"name":"bo","delayMs":1}`), Protoset: set})
	if code, err := call(t, c); code != "OK" || err != nil {
		t.Errorf("call = %q, %v; want OK", code, err)
	}
	if echo.Names()["bo"] != 1 {
		t.Errorf("the server got %v", echo.Names())
	}

	_, err := dial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say})
	if err == nil || !strings.Contains(err.Error(), "reflection") || !strings.Contains(err.Error(), "protoset") {
		t.Errorf("without reflection or a protoset: %v; want it to say so and what to do", err)
	}
}

func TestWhatCannotBeCalledIsRefusedBeforeTheTest(t *testing.T) {
	addr, echo, _ := serve(t, grpcdemo.ReflectionBoth)
	_, set := grpcdemo.Files()
	for _, tc := range []struct {
		name string
		o    Options
		says []string
	}{
		{"no such service", Options{Method: "gmeter.demo.Nope/Say"}, []string{"gmeter.demo.Nope", "gmeter.demo.Echo"}},
		{"no such service in the protoset", Options{Method: "gmeter.demo.Nope/Say", Protoset: set}, []string{"gmeter.demo.Nope", "gmeter.demo.Echo"}},
		{"no such method", Options{Method: "gmeter.demo.Echo/Shout"}, []string{"Shout", "Say"}},
		{"a streaming method", Options{Method: "gmeter.demo.Echo/Chat"}, []string{"stream"}},
		{"not a method name", Options{Method: "Say"}, []string{"Service/Method"}},
		{"no method", Options{Method: "gmeter.demo.Echo/"}, []string{"Service/Method"}},
		{"no service", Options{Method: ".Say"}, []string{"Service/Method"}},
		{"a message, not a service", Options{Method: "gmeter.demo.SayRequest/Say", Protoset: set}, []string{"gmeter.demo.SayRequest", "gmeter.demo.Echo"}},
		{"a field the request does not have", Options{Method: grpcdemo.Say, Data: []byte(`{"nme":"x"}`)}, []string{"nme"}},
		{"not JSON", Options{Method: grpcdemo.Say, Data: []byte(`{name:`)}, []string{"SayRequest"}},
		{"a broken protoset", Options{Method: grpcdemo.Say, Protoset: []byte("not a descriptor set")}, []string{"protoset"}},
		{"no connections", Options{Method: grpcdemo.Say, Conns: -1}, []string{"connections"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.o.Addr, tc.o.Plaintext = addr, true
			_, err := dial(t, tc.o)
			for _, want := range tc.says {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v; want it to mention %q", err, want)
				}
			}
		})
	}
	if n := echo.Calls.Load(); n != 0 {
		t.Errorf("%d calls reached the service", n)
	}
}

func TestACallSaysHowItEnded(t *testing.T) {
	addr, _, _ := serve(t, grpcdemo.ReflectionBoth)
	with := func(data string) *Caller {
		return mustDial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say, Data: []byte(data)})
	}
	for data, want := range map[string]string{
		`{"code":"NOT_FOUND"}`:          "NOT_FOUND",
		`{"code":"INTERNAL"}`:           "INTERNAL",
		`{"code":"RESOURCE_EXHAUSTED"}`: "RESOURCE_EXHAUSTED",
		`{"code":"DEADLINE_EXCEEDED"}`:  "DEADLINE_EXCEEDED", // the server's own answer, not this call's timeout
		`{"code":"CANCELLED"}`:          "CANCELLED",
	} {
		if code, err := call(t, with(data)); code != want || err != nil {
			t.Errorf("%s: call = %q, %v; want %s", data, code, err, want)
		}
	}

	// UNAVAILABLE is also what a broken connection says, so it is never taken for an answer.
	_, err := call(t, with(`{"code":"UNAVAILABLE"}`))
	var f runner.Failure
	if !errors.As(err, &f) || f.Kind != "unavailable" {
		t.Errorf("UNAVAILABLE: %v, want an unavailable failure", err)
	}

	slow := with(`{"delay_ms":2000}`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := slow.Call(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("past its timeout: %v, want the context's deadline", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, stop)
	if _, err := slow.Call(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("stopped: %v, want the context's cancellation", err)
	}
}

func TestACallCarriesItsMetadataAndSpreadsOverTheConnections(t *testing.T) {
	addr, echo, _ := serve(t, grpcdemo.ReflectionBoth)
	c := mustDial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say, Conns: 3, Metadata: metadata.Pairs("x-test", "yes")})
	for range 30 {
		if code, err := call(t, c); code != "OK" || err != nil {
			t.Fatalf("call = %q, %v", code, err)
		}
	}
	if got := echo.Metadata().Get("x-test"); len(got) != 1 || got[0] != "yes" {
		t.Errorf("x-test = %v, want yes", got)
	}
	if echo.Peers() != 3 {
		t.Errorf("the calls came over %d connections, want 3", echo.Peers())
	}
	if echo.Names()[""] != 30 {
		t.Errorf("names = %v, want 30 calls with the empty request", echo.Names())
	}
}

func TestAConnectionThatBreaksFailsTheCalls(t *testing.T) {
	addr, _, s := serve(t, grpcdemo.ReflectionBoth)
	c := mustDial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say})
	s.Stop()
	_, err := call(t, c)
	var f runner.Failure
	if !errors.As(err, &f) || f.Kind != "unavailable" {
		t.Errorf("err = %v, want an unavailable failure", err)
	}
}

func TestAServerThatCannotBeReachedIsNamedWithTheReason(t *testing.T) {
	cert, pool := selfSigned(t)
	tlsAddr, _, _ := serve(t, grpcdemo.ReflectionBoth, grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
	plainAddr, _, _ := serve(t, grpcdemo.ReflectionBoth)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()

	if c, err := dial(t, Options{Addr: tlsAddr, TLS: &tls.Config{RootCAs: pool}, Method: grpcdemo.Say}); err != nil {
		t.Errorf("a trusted certificate: %v", err)
	} else if code, err := call(t, c); code != "OK" || err != nil {
		t.Errorf("over TLS: %q, %v", code, err)
	}
	if _, err := dial(t, Options{Addr: tlsAddr, TLS: &tls.Config{InsecureSkipVerify: true}, Method: grpcdemo.Say}); err != nil {
		t.Errorf("verification skipped: %v", err)
	}
	for _, tc := range []struct {
		name string
		o    Options
		says string
	}{
		{"an untrusted certificate", Options{Addr: tlsAddr}, "certificate"},
		{"TLS to a server without it", Options{Addr: plainAddr}, "tls"},
		{"no TLS to a server with it", Options{Addr: tlsAddr, Plaintext: true}, tlsAddr},
		{"nothing listening", Options{Addr: closed, Plaintext: true}, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.o.Method = grpcdemo.Say
			start := time.Now()
			_, err := dial(t, tc.o)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v; want it to mention %q", err, tc.says)
			}
			if time.Since(start) > 3*time.Second {
				t.Errorf("took %v to give up", time.Since(start))
			}
		})
	}
}

// lateContext is a context whose deadline has passed but that has not yet
// marked itself done, as a real one is for a moment before its timer fires.
type lateContext struct{ context.Context }

func (lateContext) Deadline() (time.Time, bool) { return time.Now().Add(-time.Millisecond), true }

func TestACallPastItsDeadlineIsATimeoutEvenBeforeTheContextSaysSo(t *testing.T) {
	addr, _, _ := serve(t, grpcdemo.ReflectionBoth)
	c := mustDial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say})
	code, err := c.Call(lateContext{context.Background()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("call = %q, %v; want the call's own deadline, not an answer from the server", code, err)
	}
}
