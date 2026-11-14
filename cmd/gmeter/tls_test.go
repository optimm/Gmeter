package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
)

// tlsOKServer is an https server that answers 200, with a certificate that no
// system root vouches for.
func tlsOKServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // a refused handshake is the point of some tests
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// certFile writes the server's certificate as PEM, the file -cacert takes.
func certFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var quickRun = []string{"-rate", "100", "-duration", "300ms", "-workers", "4", "-quiet"}

func TestAnUntrustedCertificateIsReportedAsTLSErrors(t *testing.T) {
	srv := tlsOKServer(t)

	code, stdout, stderr := exec(t, append(quickRun, srv.URL)...)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "tls: 30") || strings.Contains(stdout, "30 ok") {
		t.Errorf("want 30 requests failing as tls errors and none answered:\n%s", stdout)
	}
}

func TestInsecureSkipsCertificateVerification(t *testing.T) {
	srv := tlsOKServer(t)

	code, stdout, stderr := exec(t, append(quickRun, "-insecure", srv.URL)...)

	if code != 0 || !strings.Contains(stdout, "30 ok") {
		t.Fatalf("exit code %d, want 30 ok:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// A run that did not verify the server must never be mistaken for one that did.
func TestInsecureSaysSoOnStderrUnlessQuiet(t *testing.T) {
	srv := tlsOKServer(t)

	_, _, loud := exec(t, "-rate", "50", "-duration", "200ms", "-insecure", srv.URL)
	_, _, quiet := exec(t, "-rate", "50", "-duration", "200ms", "-quiet", "-insecure", srv.URL)

	if !strings.Contains(loud, "certificate is not verified") {
		t.Errorf("stderr does not say the certificate is not verified:\n%s", loud)
	}
	if quiet != "" {
		t.Errorf("-quiet run wrote to stderr: %q", quiet)
	}
}

func TestCACertTrustsAPrivateAuthority(t *testing.T) {
	srv := tlsOKServer(t)

	code, stdout, stderr := exec(t, append(quickRun, "-cacert", certFile(t, srv), srv.URL)...)

	if code != 0 || !strings.Contains(stdout, "30 ok") {
		t.Fatalf("exit code %d, want 30 ok:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// someOtherAuthority writes a certificate that has nothing to do with the test
// server's, as PEM.
func someOtherAuthority(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "some other authority"},
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
	path := filepath.Join(t.TempDir(), "other.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// -cacert is the set of authorities to trust, not an addition to a policy that
// lets everything through: a server another authority vouches for is refused.
func TestCACertTrustsOnlyTheAuthoritiesItHolds(t *testing.T) {
	srv := tlsOKServer(t)

	code, stdout, stderr := exec(t, append(quickRun, "-cacert", someOtherAuthority(t), srv.URL)...)

	if code != 0 || !strings.Contains(stdout, "tls: 30") {
		t.Fatalf("exit code %d, want every request refused as a tls error:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

func TestBadTLSFlagsAreUsageErrors(t *testing.T) {
	srv := tlsOKServer(t)
	notPEM := writeTrace(t, "notes.txt", "this is not a certificate\n")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"missing file":      {[]string{"-cacert", filepath.Join(t.TempDir(), "nope.pem")}, "no such file"},
		"not a certificate": {[]string{"-cacert", notPEM}, "no certificate found in"},
		"both":              {[]string{"-insecure", "-cacert", notPEM}, "cannot be combined"},
	} {
		code, _, stderr := exec(t, append(append(append([]string{}, quickRun...), tc.args...), srv.URL)...)
		if code != 2 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit code %d, stderr %q, want 2 and a message with %q", name, code, stderr, tc.want)
		}
	}
}

func TestReplayAcceptsTheTLSFlags(t *testing.T) {
	srv := tlsOKServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)

	code, stdout, stderr := execReplay(t, "-file", trace, "-methods", "GET,POST", "-quiet", "-insecure", srv.URL)

	if code != 0 || !strings.Contains(stdout, "3 ok") {
		t.Fatalf("exit code %d, want 3 ok:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// find and replay say it too, and find's trials use the TLS configuration.
func TestFindAndReplayAnnounceInsecureAndFindUsesIt(t *testing.T) {
	fast(t)
	srv := tlsOKServer(t)

	code, out, stderr := exec(t, "find", "-slo", "p99=1s", "-start", "20", "-max", "20", "-trial", "300ms", "-warmup", "0",
		"-confirm", "0", "-arrivals", "even", "-insecure", "-json", "-", srv.URL)
	if code != 0 {
		t.Fatalf("find: exit %d:\n%s\nstderr:\n%s", code, out, stderr)
	}
	var got foundJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("find: stdout should be only JSON: %v\n%s", err, out)
	}
	if !noTrialFailed(got) {
		t.Errorf("find -insecure against a fast https server should have no failed trial: %+v", got.Result)
	}
	if !strings.Contains(stderr, "certificate is not verified") {
		t.Errorf("find did not say the certificate is not verified:\n%s", stderr)
	}

	trace := writeTrace(t, "trace.jsonl", timedTrace)
	_, _, stderr = execReplay(t, "-file", trace, "-methods", "GET,POST", "-insecure", srv.URL)
	if !strings.Contains(stderr, "certificate is not verified") {
		t.Errorf("replay did not say the certificate is not verified:\n%s", stderr)
	}
}
