package runner

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stepClock is a clock the test moves by hand.
type stepClock struct {
	base, t time.Time
}

func newStepClock() *stepClock {
	now := time.Now()
	return &stepClock{base: now, t: now}
}

func (c *stepClock) now() time.Time { return c.t }

// at moves the clock to d after the moment the request was sent.
func (c *stepClock) at(d time.Duration) { c.t = c.base.Add(d) }

const ms = time.Millisecond

func TestPhasesOfARequestThatOpenedAConnection(t *testing.T) {
	clock := newStepClock()
	c := newPhaseClock(clock.now)
	tr := c.trace()

	clock.at(2 * ms)
	tr.DNSStart(httptrace.DNSStartInfo{})
	clock.at(5 * ms)
	tr.DNSDone(httptrace.DNSDoneInfo{})
	tr.ConnectStart("tcp", "10.0.0.1:443")
	clock.at(20 * ms)
	tr.ConnectDone("tcp", "10.0.0.1:443", nil)
	tr.TLSHandshakeStart()
	clock.at(50 * ms)
	tr.TLSHandshakeDone(tls.ConnectionState{}, nil)
	clock.at(52 * ms)
	tr.GotConn(httptrace.GotConnInfo{Reused: false})
	clock.at(55 * ms)
	tr.WroteRequest(httptrace.WroteRequestInfo{})
	clock.at(155 * ms)
	tr.GotFirstResponseByte()
	clock.at(355 * ms)

	intended := clock.base.Add(-25 * ms) // it waited 25ms for a worker
	got := c.split(intended, clock.base, clock.t)

	want := phaseTimes{queue: 25 * ms, dns: 3 * ms, connect: 15 * ms, tls: 30 * ms, send: 7 * ms, wait: 100 * ms, transfer: 200 * ms}
	if got != want {
		t.Errorf("phases = %+v, want %+v", got, want)
	}
	if got.sum() != clock.t.Sub(intended) {
		t.Errorf("the phases add up to %v, the corrected latency is %v: they must split it exactly", got.sum(), clock.t.Sub(intended))
	}
}

func TestAReusedConnectionHasNoConnectionPhasesEvenIfItsOwnDialFinishedLate(t *testing.T) {
	// A request starts a dial, then an idle connection becomes free and the
	// request uses that instead. The dial did not delay it.
	clock := newStepClock()
	c := newPhaseClock(clock.now)
	tr := c.trace()
	clock.at(1 * ms)
	tr.DNSStart(httptrace.DNSStartInfo{})
	clock.at(4 * ms)
	tr.DNSDone(httptrace.DNSDoneInfo{})
	tr.ConnectStart("tcp", "x")
	clock.at(9 * ms)
	tr.ConnectDone("tcp", "x", nil)
	tr.GotConn(httptrace.GotConnInfo{Reused: true})
	tr.WroteRequest(httptrace.WroteRequestInfo{})
	clock.at(20 * ms)
	tr.GotFirstResponseByte()
	clock.at(30 * ms)

	got := c.split(clock.base, clock.base, clock.t)

	if got.dns != 0 || got.connect != 0 || got.tls != 0 {
		t.Errorf("dns/connect/tls = %v/%v/%v, want 0: this request used a connection that already existed", got.dns, got.connect, got.tls)
	}
	if got.sum() != 30*ms {
		t.Errorf("the phases add up to %v, want 30ms", got.sum())
	}
}

func TestConnectSpansAllTheAttemptsUpToTheOneThatSucceeded(t *testing.T) {
	// Dialing a name with several addresses tries them in turn, or at once.
	clock := newStepClock()
	c := newPhaseClock(clock.now)
	tr := c.trace()
	tr.ConnectStart("tcp", "[::1]:80")
	clock.at(10 * ms)
	tr.ConnectStart("tcp", "127.0.0.1:80")
	clock.at(15 * ms)
	tr.ConnectDone("tcp", "[::1]:80", errors.New("refused"))
	clock.at(40 * ms)
	tr.ConnectDone("tcp", "127.0.0.1:80", nil)
	tr.GotConn(httptrace.GotConnInfo{})

	got := c.split(clock.base, clock.base, clock.t)

	if got.connect != 40*ms {
		t.Errorf("connect = %v, want 40ms: from the first attempt to the success, not to the failure", got.connect)
	}
}

func TestAFailedHandshakeOrDialIsNotAPhase(t *testing.T) {
	clock := newStepClock()
	c := newPhaseClock(clock.now)
	tr := c.trace()
	tr.ConnectStart("tcp", "x")
	clock.at(5 * ms)
	tr.ConnectDone("tcp", "x", errors.New("refused"))
	tr.TLSHandshakeStart()
	clock.at(9 * ms)
	tr.TLSHandshakeDone(tls.ConnectionState{}, errors.New("bad certificate"))

	got := c.split(clock.base, clock.base, clock.t)

	if got.connect != 0 || got.tls != 0 {
		t.Errorf("connect/tls = %v/%v, want 0 for attempts that failed", got.connect, got.tls)
	}
}

func TestAnEarlyResponseNeverMakesTheWaitNegative(t *testing.T) {
	// A server can answer before it has read the whole request, so the first
	// byte can come before the request is fully written.
	clock := newStepClock()
	c := newPhaseClock(clock.now)
	tr := c.trace()
	tr.GotConn(httptrace.GotConnInfo{Reused: true})
	clock.at(20 * ms)
	tr.GotFirstResponseByte()
	clock.at(30 * ms)
	tr.WroteRequest(httptrace.WroteRequestInfo{})
	clock.at(50 * ms)

	got := c.split(clock.base, clock.base, clock.t)

	if got.wait != 0 || got.transfer != 30*ms || got.send != 20*ms {
		t.Errorf("phases = %+v, want wait 0, transfer 30ms, send 20ms", got)
	}
	if got.sum() != 50*ms {
		t.Errorf("the phases add up to %v, want 50ms", got.sum())
	}
}

func TestWithNoHooksTheWholeServiceTimeIsSend(t *testing.T) {
	clock := newStepClock()
	c := newPhaseClock(clock.now)
	clock.at(40 * ms)

	got := c.split(clock.base.Add(-10*ms), clock.base, clock.t)

	if got != (phaseTimes{queue: 10 * ms, send: 40 * ms}) {
		t.Errorf("phases = %+v, want queue 10ms and send 40ms", got)
	}
}

// The transport can run a hook after the request has returned (a dial that lost
// a race to an idle connection). With -race this fails if that is not safe.
func TestHooksMayFireWhileTheRequestIsBeingSplit(t *testing.T) {
	c := newPhaseClock(time.Now)
	tr := c.trace()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			tr.DNSStart(httptrace.DNSStartInfo{})
			tr.DNSDone(httptrace.DNSDoneInfo{})
			tr.ConnectStart("tcp", "x")
			tr.ConnectDone("tcp", "x", nil)
			tr.TLSHandshakeStart()
			tr.TLSHandshakeDone(tls.ConnectionState{}, nil)
		}
	})
	for range 1000 {
		now := time.Now()
		c.split(now, now, time.Now())
	}
	wg.Wait()
}

// The same hooks on a real transport. Each phase is made slow in a different
// way, and the assertions leave a wide margin so that a busy machine does not
// fail them: they check each time lands in its own phase, not how long it took.
func TestPhasesOfARealRequestOnANewConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * ms) // before the first byte
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		time.Sleep(400 * ms) // between the first byte and the end
		io.WriteString(w, "second")
	}))
	t.Cleanup(srv.Close)
	slowDial := &net.Dialer{Control: func(string, string, syscall.RawConn) error {
		time.Sleep(40 * ms) // inside the TCP connect
		return nil
	}}
	client := &http.Client{Transport: &http.Transport{DialContext: slowDial.DialContext}}
	t.Cleanup(client.CloseIdleConnections)

	got, total := timedGet(t, client, srv.URL)

	if got.connect < 40*ms || got.connect > 150*ms {
		t.Errorf("connect = %v, want at least the 40ms the dial took and well under the server's delays", got.connect)
	}
	if got.wait < 200*ms || got.wait > 380*ms {
		t.Errorf("wait = %v, want the server's 200ms before the first byte and not its 400ms after it", got.wait)
	}
	if got.transfer < 400*ms || got.transfer > 550*ms {
		t.Errorf("transfer = %v, want the 400ms between the first byte and the end, and not the 200ms before it", got.transfer)
	}
	if got.dns != 0 || got.tls != 0 {
		t.Errorf("dns/tls = %v/%v, want 0: an IP address and plain HTTP", got.dns, got.tls)
	}
	if got.send < 0 || got.send > 100*ms {
		t.Errorf("send = %v, want a small remainder", got.send)
	}
	if got.sum() != total {
		t.Errorf("the phases add up to %v, the request took %v", got.sum(), total)
	}
}

func TestASecondRequestOnTheSameConnectionHasNoConnectionPhases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	first, _ := timedGet(t, client, srv.URL)
	second, _ := timedGet(t, client, srv.URL)

	if first.connect == 0 {
		t.Errorf("the first request opened a connection but connect = 0")
	}
	if second.dns != 0 || second.connect != 0 || second.tls != 0 {
		t.Errorf("second request dns/connect/tls = %v/%v/%v, want 0: the connection was reused", second.dns, second.connect, second.tls)
	}
}

func TestTLSIsTimedOnlyForHTTPS(t *testing.T) {
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(secure.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(plain.Close)

	https, _ := timedGet(t, secure.Client(), secure.URL)
	http1, _ := timedGet(t, &http.Client{Transport: &http.Transport{}}, plain.URL)

	if https.tls <= 0 || https.connect <= 0 {
		t.Errorf("https: tls/connect = %v/%v, want both measured", https.tls, https.connect)
	}
	if http1.tls != 0 {
		t.Errorf("plain http: tls = %v, want 0", http1.tls)
	}
}

func TestDNSIsTimedForAHostNameButNotForAnAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	byName, _ := timedGet(t, &http.Client{Transport: &http.Transport{}}, "http://localhost:"+port)
	byAddress, _ := timedGet(t, &http.Client{Transport: &http.Transport{}}, srv.URL)

	if byName.dns <= 0 {
		t.Errorf("a host name: dns = %v, want it measured", byName.dns)
	}
	if byAddress.dns != 0 {
		t.Errorf("an IP address: dns = %v, want 0, there is nothing to look up", byAddress.dns)
	}
}

// timedGet sends one request with the trace attached, as a worker does, pretending
// it waited 25ms for a worker. It returns the phases and the corrected latency.
func timedGet(t *testing.T, client *http.Client, url string) (phaseTimes, time.Duration) {
	t.Helper()
	c := newPhaseClock(time.Now)
	ctx := httptrace.WithClientTrace(context.Background(), c.trace())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	finished := time.Now()
	intended := sent.Add(-25 * ms)
	return c.split(intended, sent, finished), finished.Sub(intended)
}

// The transport retries a request on a connection the server had closed, so the
// request can be written twice; the wait is for the answer to the last write.
func TestWaitIsMeasuredFromTheLastTimeTheRequestWasWritten(t *testing.T) {
	clock := newStepClock()
	c := newPhaseClock(clock.now)
	tr := c.trace()
	tr.GotConn(httptrace.GotConnInfo{Reused: true})
	clock.at(5 * ms)
	tr.WroteRequest(httptrace.WroteRequestInfo{}) // on the stale connection
	clock.at(25 * ms)
	tr.GotConn(httptrace.GotConnInfo{Reused: false}) // the retry
	clock.at(30 * ms)
	tr.WroteRequest(httptrace.WroteRequestInfo{})
	clock.at(80 * ms)
	tr.GotFirstResponseByte()
	clock.at(90 * ms)

	got := c.split(clock.base, clock.base, clock.t)

	if got.wait != 50*ms || got.transfer != 10*ms {
		t.Errorf("wait/transfer = %v/%v, want 50ms/10ms", got.wait, got.transfer)
	}
}

// What the trace costs per request, against a loopback server: compare the two.
//
//	go test -run '^$' -bench PhaseTrace -benchmem ./internal/runner
func benchmarkRequest(b *testing.B, traced bool) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	b.Cleanup(srv.Close)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
	b.Cleanup(client.CloseIdleConnections)
	b.ReportAllocs()
	for b.Loop() {
		ctx := context.Background()
		var c *phaseClock
		if traced {
			c = newPhaseClock(time.Now)
			ctx = httptrace.WithClientTrace(ctx, c.trace())
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		sent := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if c != nil {
			c.split(sent, sent, time.Now())
		}
	}
}

func BenchmarkPhaseTraceOff(b *testing.B) { benchmarkRequest(b, false) }
func BenchmarkPhaseTraceOn(b *testing.B)  { benchmarkRequest(b, true) }
