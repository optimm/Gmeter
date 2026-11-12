package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// tlsServer is an https server whose certificate no system root vouches for.
func tlsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // a refused handshake is the point of some tests
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestAnUntrustedCertificateIsRefusedByDefault(t *testing.T) {
	srv := tlsServer(t)

	res, err := Run(context.Background(), baseConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Statuses[200] != 0 {
		t.Fatalf("%d requests were answered although the server's certificate is not trusted", res.Statuses[200])
	}
	// Not "other": the report has to say why every request failed.
	if res.Errors["tls"] != res.Scheduled || res.Scheduled == 0 {
		t.Errorf("errors = %v, want tls=%d", res.Errors, res.Scheduled)
	}
}

func TestClassifyNamesCertificateAndHandshakeFailuresAsTLS(t *testing.T) {
	wrapped := func(err error) error { return &url.Error{Op: "Get", URL: "https://x", Err: err} }
	for name, err := range map[string]error{
		// Whatever the cause (here one no other case below knows), a certificate that failed verification is a TLS failure.
		"certificate verification": wrapped(&tls.CertificateVerificationError{Err: errors.New("x509: signature algorithm is insecure")}),
		"unknown authority":        wrapped(x509.UnknownAuthorityError{}),
		"wrong host name":          wrapped(x509.HostnameError{Host: "x"}),
		"expired certificate":      wrapped(x509.CertificateInvalidError{Reason: x509.Expired}),
		"server speaks plain HTTP": wrapped(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}),
		"peer rejected us":         wrapped(tls.AlertError(42)),
	} {
		if got := classify(err); got != "tls" {
			t.Errorf("%s: classified as %q, want tls", name, got)
		}
	}
	// What is not a TLS failure keeps its class.
	if got := classify(wrapped(&net.DNSError{})); got != "dns" {
		t.Errorf("a DNS failure is %q, want dns", got)
	}
	if got := classify(errors.New("boom")); got != "other" {
		t.Errorf("an unknown error is %q, want other", got)
	}
}

func TestATrustedRootLetsRequestsToAPrivateCAThrough(t *testing.T) {
	srv := tlsServer(t)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	cfg := baseConfig(srv.URL)
	cfg.TLS = &tls.Config{RootCAs: roots}

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Statuses[200] != res.Scheduled || res.Scheduled == 0 {
		t.Errorf("%d of %d requests were answered, errors %v: want all, the certificate is trusted", res.Statuses[200], res.Scheduled, res.Errors)
	}
}

func TestSkippingVerificationLetsRequestsToAnUntrustedServerThrough(t *testing.T) {
	srv := tlsServer(t)
	cfg := baseConfig(srv.URL)
	cfg.TLS = &tls.Config{InsecureSkipVerify: true}

	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Statuses[200] != res.Scheduled || res.Scheduled == 0 {
		t.Errorf("%d of %d requests were answered, errors %v: want all", res.Statuses[200], res.Scheduled, res.Errors)
	}
}

// A TLS configuration must not bring HTTP/2 back: the phases depend on a
// connection serving one request at a time.
func TestATLSConfigurationStaysOnHTTP11(t *testing.T) {
	var mu sync.Mutex
	var protos []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.Proto)
		mu.Unlock()
	}))
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cfg := baseConfig(srv.URL)
	cfg.Requests = labelled("/a")
	cfg.TLS = &tls.Config{InsecureSkipVerify: true}

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(protos) == 0 {
		t.Fatal("the server saw no request")
	}
	for _, p := range protos {
		if p != "HTTP/1.1" {
			t.Fatalf("the server saw %s, want HTTP/1.1 although it offers HTTP/2", p)
		}
	}
}

// An https test server can only be reached with a TLS configuration, and Go
// leaves HTTP/2 off whenever there is one, so the default client (no
// configuration) is checked on its transport: HTTP/2 is switched off explicitly.
func TestTheClientNeverOffersHTTP2(t *testing.T) {
	for name, cfg := range map[string]*tls.Config{
		"without a TLS configuration": nil,
		"with a TLS configuration":    {InsecureSkipVerify: true},
	} {
		tr := newClient(4, cfg).Transport.(*http.Transport)
		if tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 || tr.ForceAttemptHTTP2 {
			t.Errorf("%s: TLSNextProto = %v, ForceAttemptHTTP2 = %v, want an empty map and false: the phases need one request at a time on a connection",
				name, tr.TLSNextProto, tr.ForceAttemptHTTP2)
		}
	}
}
