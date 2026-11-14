package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
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

// clientAuthority is a certificate authority that signs client certificates,
// for the servers that insist on one.
type clientAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newClientAuthority(t *testing.T) *clientAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test client authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &clientAuthority{cert: cert, key: key, pool: pool}
}

// issue writes a client certificate this authority signed and its private key,
// as the PEM files -cert and -key take.
func (a *clientAuthority) issue(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "gmeter test client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "client.pem")
	keyPath = filepath.Join(dir, "client-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// mutualTLSServer is an https server that answers 200 only to a client that
// shows a certificate a vouches for, and the file -cacert needs to trust it.
func mutualTLSServer(t *testing.T, a *clientAuthority) (srv *httptest.Server, ca string) {
	t.Helper()
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // a refused handshake is the point of some tests
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: a.pool}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, certFile(t, srv)
}

func TestACertificateAndKeyLetTheClientIntoAServerThatDemandsOne(t *testing.T) {
	a := newClientAuthority(t)
	srv, ca := mutualTLSServer(t, a)
	cert, key := a.issue(t)

	code, stdout, stderr := exec(t, append(quickRun, "-cacert", ca, srv.URL)...)
	if code != 0 || strings.Contains(stdout, "30 ok") {
		t.Fatalf("without a certificate: exit code %d, want the requests refused:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	code, stdout, stderr = exec(t, append(quickRun, "-cacert", ca, "-cert", cert, "-key", key, srv.URL)...)
	if code != 0 || !strings.Contains(stdout, "30 ok") {
		t.Fatalf("with -cert and -key: exit code %d, want 30 ok:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

func TestAClientCertificateFromAnotherAuthorityIsRefused(t *testing.T) {
	srv, ca := mutualTLSServer(t, newClientAuthority(t))
	cert, key := newClientAuthority(t).issue(t) // signed by an authority the server does not know

	code, stdout, stderr := exec(t, append(quickRun, "-cacert", ca, "-cert", cert, "-key", key, srv.URL)...)

	if code != 0 || strings.Contains(stdout, "30 ok") {
		t.Fatalf("exit code %d, want the requests refused:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

func TestReplayAndFindSendTheClientCertificateToo(t *testing.T) {
	fast(t)
	a := newClientAuthority(t)
	srv, ca := mutualTLSServer(t, a)
	cert, key := a.issue(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)

	code, stdout, stderr := execReplay(t, "-file", trace, "-methods", "GET,POST", "-quiet", "-cacert", ca, "-cert", cert, "-key", key, srv.URL)
	if code != 0 || strings.Contains(stdout, "tls:") || !strings.Contains(stdout, "3 ok") {
		t.Errorf("replay: exit code %d, want every request answered:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	code, out, stderr := exec(t, "find", "-slo", "p99=1s", "-start", "20", "-max", "20", "-trial", "300ms", "-warmup", "0",
		"-confirm", "0", "-arrivals", "even", "-cacert", ca, "-cert", cert, "-key", key, "-json", "-", srv.URL)
	var got foundJSON
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || !noTrialFailed(got) {
		t.Errorf("find: exit code %d, want no failed trial with the certificate:\n%s\nstderr:\n%s", code, out, stderr)
	}
}

func TestGRPCSendsTheClientCertificate(t *testing.T) {
	a := newClientAuthority(t)
	cert, key := a.issue(t)
	https := httptest.NewUnstartedServer(nil) // only for its certificate
	https.StartTLS()
	t.Cleanup(https.Close)
	creds := credentials.NewTLS(&tls.Config{
		Certificates: https.TLS.Certificates,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    a.pool,
	})
	addr, _ := echoServer(t, grpcdemo.ReflectionBoth, grpc.Creds(creds))
	ca := certFile(t, https)
	quick := []string{"-call", grpcdemo.Say, "-rate", "50", "-duration", "200ms"}

	if run, _, _ := grpcSaved(t, append(quick, "-quiet", "-cacert", ca, "-cert", cert, "-key", key, addr)...); run.OK != 10 {
		t.Errorf("with -cert and -key: %d ok, want 10", run.OK)
	}
	if code, _, stderr := exec(t, append([]string{"grpc"}, append(quick, "-quiet", "-cacert", ca, addr)...)...); code == 0 {
		t.Errorf("without a certificate the server should refuse every call, stderr:\n%s", stderr)
	}
}

func TestBadClientCertificateFlagsAreUsageErrors(t *testing.T) {
	a := newClientAuthority(t)
	srv, _ := mutualTLSServer(t, a)
	cert, key := a.issue(t)
	otherCert, _ := a.issue(t)
	notPEM := writeTrace(t, "notes.txt", "this is not a certificate\n")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"cert alone":        {[]string{"-cert", cert}, "-cert and -key go together"},
		"key alone":         {[]string{"-key", key}, "-cert and -key go together"},
		"missing cert file": {[]string{"-cert", filepath.Join(t.TempDir(), "nope.pem"), "-key", key}, "nope.pem"},
		"not a pair":        {[]string{"-cert", otherCert, "-key", key}, "does not match"},
		"not PEM":           {[]string{"-cert", notPEM, "-key", key}, "-cert"},
		"with agents":       {[]string{"-cert", cert, "-key", key, "-agents", "127.0.0.1:1"}, "private key"},
	} {
		t.Setenv(tokenEnv, "a-long-enough-password")
		code, _, stderr := exec(t, append(append(append([]string{}, quickRun...), tc.args...), srv.URL)...)
		if code != 2 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit code %d, stderr %q, want 2 and a message with %q", name, code, stderr, tc.want)
		}
	}
}

func TestPlaintextHasNoClientCertificateToSend(t *testing.T) {
	a := newClientAuthority(t)
	cert, key := a.issue(t)

	code, _, stderr := exec(t, "grpc", "-call", grpcdemo.Say, "-plaintext", "-cert", cert, "-key", key, "127.0.0.1:1")

	if code != 2 || !strings.Contains(stderr, "-plaintext") {
		t.Errorf("exit code %d, stderr %q, want 2 and a message about -plaintext", code, stderr)
	}
}
